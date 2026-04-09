package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Domain      string
	DomainsFile string
	OutputDir   string
	OutputFmt   string
	TGToken     string
	TGChatID    string
	Workers     int
}

type DomainResult struct {
	Domain string
	Count  int
	Err    error
}

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.Domain, "domain", "", "Single domain to scan")
	flag.StringVar(&cfg.DomainsFile, "file", "", "File containing one domain per line")
	flag.StringVar(&cfg.OutputDir, "output", "results", "Directory to write output files")
	flag.StringVar(&cfg.OutputFmt, "format", "txt", "Output format: txt or csv")
	flag.StringVar(&cfg.TGToken, "tg-token", "", "Telegram Bot Token")
	flag.StringVar(&cfg.TGChatID, "tg-chat", "", "Telegram Chat ID")
	flag.IntVar(&cfg.Workers, "workers", 3, "Concurrent workers")
	flag.Parse()

	if cfg.TGToken == "" {
		cfg.TGToken = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	if cfg.TGChatID == "" {
		cfg.TGChatID = os.Getenv("TELEGRAM_CHAT_ID")
	}

	domains, err := collectDomains(cfg)
	if err != nil || len(domains) == 0 {
		fmt.Println("❌ No domains provided.")
		os.Exit(1)
	}

	fmt.Printf("🎯 Scanning %d domain(s) with waybackurls...\n\n", len(domains))
	
	msg := fmt.Sprintf("🕵️ *WaybackRecon Started*\n\n📋 Domains: %d\n⏰ Started: %s\n⏳ Chunk Size: 5000 URLs",
		len(domains), time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
	sendTelegram(cfg, msg, false, "")

	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		fmt.Printf("❌ Cannot create output dir: %v\n", err)
		os.Exit(1)
	}

	jobs := make(chan string, len(domains))
	results := make(chan DomainResult, len(domains))

	workers := cfg.Workers
	if workers < 1 { workers = 1 }

	ts := time.Now().UTC().Format("20060102_150405")

	for w := 0; w < workers; w++ {
		go func() {
			for domain := range jobs {
				count, err := runWaybackChunked(cfg, domain, ts)
				results <- DomainResult{Domain: domain, Count: count, Err: err}
			}
		}()
	}

	for _, d := range domains {
		jobs <- d
	}
	close(jobs)

	totalURLs := 0
	for i := 0; i < len(domains); i++ {
		res := <-results
		totalURLs += res.Count
		if res.Err != nil && res.Count == 0 {
			fmt.Printf("⚠️  [%s] Error: %v\n", res.Domain, res.Err)
		} else {
			fmt.Printf("✅ [%s] Found %d URLs\n", res.Domain, res.Count)
		}
	}

	summary := fmt.Sprintf(
		"✅ *WaybackRecon Complete!*\n\n📋 Domains scanned: *%d*\n🔗 Total URLs found: *%d*\n⏰ Finished: %s",
		len(domains), totalURLs,
		time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
	)

	sendTelegram(cfg, summary, false, "")
	fmt.Printf("\n🎉 Done! %d URLs from %d domains\n", totalURLs, len(domains))
}

func runWaybackChunked(cfg Config, domain string, ts string) (int, error) {
	fmt.Printf("🔍 Running waybackurls for: %s (Chunking 5000 URLs)\n", domain)

	safeDomain := strings.ReplaceAll(domain, ".", "_")
	ext := cfg.OutputFmt
	if ext != "csv" && ext != "txt" { ext = "txt" }

	cmd := exec.Command("waybackurls", domain)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}

	if err := cmd.Start(); err != nil {
		return 0, err
	}

	partNum := 1
	filename := filepath.Join(cfg.OutputDir, fmt.Sprintf("%s_p%d_%s.%s", safeDomain, partNum, ts, ext))
	f, _ := os.Create(filename)

	count := 0
	totalCount := 0
	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		u := strings.TrimSpace(scanner.Text())
		if u != "" {
			count++
			totalCount++
			f.WriteString(u + "\n")

			if count >= 5000 {
				f.Close()
				caption := fmt.Sprintf("📁 %s_p%d.%s (%d URLs)", safeDomain, partNum, ext, count)
				fmt.Printf("📤 Sending Chunk Part %d (%d URLs)...\n", partNum, count)
				sendTelegramFile(cfg, filename, caption)

				// Start new chunk
				partNum++
				count = 0
				filename = filepath.Join(cfg.OutputDir, fmt.Sprintf("%s_p%d_%s.%s", safeDomain, partNum, ts, ext))
				f, _ = os.Create(filename)
			}
		}
	}

	cmdErr := cmd.Wait()

	// Send final chunk
	if count > 0 {
		f.Close()
		caption := fmt.Sprintf("📁 %s_p%d.%s (Last Chunk: %d URLs)", safeDomain, partNum, ext, count)
		fmt.Printf("📤 Sending Final Chunk Part %d (%d URLs)...\n", partNum, count)
		sendTelegramFile(cfg, filename, caption)
	} else {
		f.Close()
	}

	if totalCount > 0 {
		return totalCount, nil
	}
	return 0, cmdErr
}

func collectDomains(cfg Config) ([]string, error) {
	var domains []string
	if cfg.Domain != "" {
		for _, d := range strings.Split(cfg.Domain, ",") {
			if strings.TrimSpace(d) != "" {
				domains = append(domains, strings.TrimSpace(d))
			}
		}
	}
	if cfg.DomainsFile != "" {
		f, err := os.Open(cfg.DomainsFile)
		if err != nil { return nil, err }
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" { domains = append(domains, line) }
		}
	}
	seen := make(map[string]bool)
	unique := []string{}
	for _, d := range domains {
		if !seen[d] {
			seen[d] = true
			unique = append(unique, d)
		}
	}
	return unique, nil
}

func sendTelegram(cfg Config, text string, isFile bool, filePath string) {
	if cfg.TGToken == "" || cfg.TGChatID == "" { return }
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.TGToken)
	payload := map[string]string{
		"chat_id":    cfg.TGChatID,
		"text":       text,
		"parse_mode": "Markdown",
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(apiURL, "application/json", bytes.NewReader(body))
	if err == nil { resp.Body.Close() }
}

func sendTelegramFile(cfg Config, filePath, caption string) {
	if cfg.TGToken == "" || cfg.TGChatID == "" { return }
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", cfg.TGToken)
	f, err := os.Open(filePath)
	if err != nil { return }
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("chat_id", cfg.TGChatID)
	mw.WriteField("caption", caption)
	
	fw, err := mw.CreateFormFile("document", filepath.Base(filePath))
	if err == nil { io.Copy(fw, f) }
	mw.Close()

	req, _ := http.NewRequest("POST", apiURL, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
