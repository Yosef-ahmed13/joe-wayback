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
	File   string
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

	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		fmt.Printf("❌ Cannot create output dir: %v\n", err)
		os.Exit(1)
	}

	jobs := make(chan string, len(domains))
	results := make(chan DomainResult, len(domains))

	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}

	ts := time.Now().UTC().Format("20060102_150405")

	for w := 0; w < workers; w++ {
		go func() {
			for domain := range jobs {
				count, file, err := runWaybackurls(cfg, domain, ts)
				results <- DomainResult{Domain: domain, Count: count, File: file, Err: err}
			}
		}()
	}

	for _, d := range domains {
		jobs <- d
	}
	close(jobs)

	totalURLs := 0
	var allFiles []string

	for i := 0; i < len(domains); i++ {
		res := <-results
		totalURLs += res.Count

		if res.Err != nil && res.Count == 0 {
			fmt.Printf("⚠️  [%s] Error: %v\n", res.Domain, res.Err)
			continue
		}

		fmt.Printf("✅ [%s] Found %d URLs\n", res.Domain, res.Count)
		if res.Count > 0 && res.File != "" {
			allFiles = append(allFiles, res.File)
		}
	}

	summary := fmt.Sprintf(
		"✅ *WaybackRecon Complete!*\n\n📋 Domains scanned: *%d*\n🔗 Total URLs found: *%d*\n📁 Files: *%d*\n⏰ Finished: %s",
		len(domains), totalURLs, len(allFiles),
		time.Now().UTC().Format("2006-01-02 1504:05 UTC"),
	)

	sendTelegram(cfg, summary, false, "")
	for _, f := range allFiles {
		caption := fmt.Sprintf("📁 %s", filepath.Base(f))
		sendTelegramFile(cfg, f, caption)
	}

	fmt.Printf("\n🎉 Done! %d URLs from %d domains\n", totalURLs, len(domains))
}

func runWaybackurls(cfg Config, domain string, ts string) (int, string, error) {
	fmt.Printf("🔍 Running waybackurls for: %s\n", domain)

	cmd := exec.Command("sh", "-c", "echo '"+domain+"' | waybackurls")
	out, err := cmd.Output()
	if err != nil {
		return 0, "", err
	}

	lines := strings.Split(string(out), "\n")
	var urls []string
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l != "" {
			urls = append(urls, l)
		}
	}

	if len(urls) == 0 {
		return 0, "", nil
	}

	safeDomain := strings.ReplaceAll(domain, ".", "_")
	ext := cfg.OutputFmt
	if ext != "csv" && ext != "txt" {
		ext = "txt"
	}

	filename := filepath.Join(cfg.OutputDir, fmt.Sprintf("%s_%s.%s", safeDomain, ts, ext))
	f, err := os.Create(filename)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	if ext == "csv" {
		f.WriteString("domain,url\n")
		for _, u := range urls {
			f.WriteString(fmt.Sprintf("%s,%s\n", domain, u))
		}
	} else {
		f.WriteString(fmt.Sprintf("# waybackurls results for: %s\n\n", domain))
		for _, u := range urls {
			f.WriteString(fmt.Sprintf("%s\n", u))
		}
	}

	return len(urls), filename, nil
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
		if err != nil {
			return nil, err
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				domains = append(domains, line)
			}
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
	if cfg.TGToken == "" || cfg.TGChatID == "" {
		return
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.TGToken)
	payload := map[string]string{
		"chat_id":    cfg.TGChatID,
		"text":       text,
		"parse_mode": "Markdown",
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(apiURL, "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}

func sendTelegramFile(cfg Config, filePath, caption string) {
	if cfg.TGToken == "" || cfg.TGChatID == "" {
		return
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", cfg.TGToken)
	
	f, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer f.Close()
	
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("chat_id", cfg.TGChatID)
	mw.WriteField("caption", caption)
	
	fw, err := mw.CreateFormFile("document", filepath.Base(filePath))
	if err == nil {
		io.Copy(fw, f)
	}
	mw.Close()
	
	req, _ := http.NewRequest("POST", apiURL, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
