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

// ─── Config ──────────────────────────────────────────────────────────────────

type Config struct {
	Domain      string
	DomainsFile string
	OutputDir   string
	OutputFmt   string // "txt" | "csv"
	TGToken     string
	TGChatID    string
	Workers     int
}

// ─── Result ───────────────────────────────────────────────────────────────────

type DomainResult struct {
	Domain string
	Count  int
	File   string
	Err    error
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	cfg := Config{}

	flag.StringVar(&cfg.Domain, "domain", "", "Single domain to scan (e.g. example.com)")
	flag.StringVar(&cfg.DomainsFile, "file", "", "File containing one domain per line")
	flag.StringVar(&cfg.OutputDir, "output", "results", "Directory to write output files")
	flag.StringVar(&cfg.OutputFmt, "format", "txt", "Output format: txt or csv")
	flag.StringVar(&cfg.TGToken, "tg-token", "", "Telegram Bot Token")
	flag.StringVar(&cfg.TGChatID, "tg-chat", "", "Telegram Chat ID")
	flag.IntVar(&cfg.Workers, "workers", 3, "Concurrent workers (default 3)")
	flag.Parse()

	if cfg.TGToken == "" {
		cfg.TGToken = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	if cfg.TGChatID == "" {
		cfg.TGChatID = os.Getenv("TELEGRAM_CHAT_ID")
	}

	domains, err := collectDomains(cfg)
	if err != nil {
		fatalf("❌ Could not read domains: %v\n", err)
	}
	if len(domains) == 0 {
		fatalf("❌ No domains provided. Use -domain or -file\n")
	}

	fmt.Printf("🎯 Scanning %d domain(s) with gau...\n\n", len(domains))
	sendTelegram(cfg, fmt.Sprintf("🕵️ *WaybackRecon Started*\n\n📋 Domains: %d\n⏰ Started: %s\n⚡ Memory Mode: Streaming to Disk",
		len(domains), time.Now().UTC().Format("2006-01-02 15:04:05 UTC")), false, "")

	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		fatalf("❌ Cannot create output dir: %v\n", err)
	}

	jobs := make(chan string, len(domains))
	results := make(chan DomainResult, len(domains))

	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(domains) {
		workers = len(domains)
	}

	ts := time.Now().UTC().Format("20060102_150405")

	for w := 0; w < workers; w++ {
		go func() {
			for domain := range jobs {
				count, file, err := streamWaybackurls(cfg, domain, ts)
				results <- DomainResult{Domain: domain, Count: count, File: file, Err: err}
			}
		}()
	}

	for _, d := range domains {
		jobs <- d
	}
	close(jobs)

	var allFiles []string
	totalURLs := 0

	for i := 0; i < len(domains); i++ {
		res := <-results

		if res.Err != nil && res.Count == 0 {
			fmt.Printf("⚠️  [%s] Error: %v\n", res.Domain, res.Err)
			continue
		}

		fmt.Printf("✅ [%s] Found %d URLs\n", res.Domain, res.Count)
		totalURLs += res.Count

		if res.Count > 0 && res.File != "" {
			allFiles = append(allFiles, res.File)
		}
	}

	summary := fmt.Sprintf(
		"✅ *WaybackRecon Complete!*\n\n"+
			"📋 Domains scanned: *%d*\n"+
			"🔗 Total URLs found: *%d*\n"+
			"📁 Files: *%d*\n"+
			"⏰ Finished: %s",
		len(domains), totalURLs, len(allFiles),
		time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
	)

	if cfg.TGToken != "" && cfg.TGChatID != "" {
		sendTelegram(cfg, summary, false, "")
		for _, f := range allFiles {
			caption := fmt.Sprintf("📁 %s", filepath.Base(f))
			sendTelegram(cfg, caption, true, f)
		}
	} else {
		fmt.Println(summary)
	}

	fmt.Printf("\n🎉 Done! %d URLs from %d domains\n", totalURLs, len(domains))
}

// ─── Stream function (bypasses memory issues) ──────────────────────────────

func streamWaybackurls(cfg Config, domain string, ts string) (int, string, error) {
	fmt.Printf("🔍 Running gau for: %s (streaming)\n", domain)

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
	} else {
		f.WriteString(fmt.Sprintf("# wayback urls (gau) results for: %s\n\n", domain))
	}

	cmd := exec.Command("gau", "--threads", "5", domain)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, "", err
	}

	if err := cmd.Start(); err != nil {
		return 0, "", err
	}

	count := 0
	scanner := bufio.NewScanner(stdout)
	// Increase buffer size for extremely long URLs sometimes returned
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		u := strings.TrimSpace(scanner.Text())
		if u != "" {
			count++
			if ext == "csv" {
				f.WriteString(fmt.Sprintf("%s,%s\n", domain, u))
			} else {
				f.WriteString(fmt.Sprintf("%s\n", u))
			}
		}
	}

	// This may return an error if Wayback times out after printing partial results.
	// But count will have whatever we successfully read!
	cmdErr := cmd.Wait()

	if count > 0 {
		return count, filename, nil
	}

	return 0, "", cmdErr
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func collectDomains(cfg Config) ([]string, error) {
	var domains []string
	if cfg.Domain != "" {
		for _, d := range strings.Split(cfg.Domain, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				domains = append(domains, d)
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
			if line != "" && !strings.HasPrefix(line, "#") {
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

// ─── Telegram ─────────────────────────────────────────────────────────────────

func sendTelegram(cfg Config, text string, isFile bool, filePath string) {
	if cfg.TGToken == "" || cfg.TGChatID == "" {
		return
	}
	if isFile && filePath != "" {
		sendTelegramFile(cfg, filePath, text)
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
	if err != nil {
		fmt.Printf("⚠️  Telegram send error: %v\n", err)
		return
	}
	defer resp.Body.Close()
}

func sendTelegramFile(cfg Config, filePath, caption string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", cfg.TGToken)
	f, err := os.Open(filePath)
	if err != nil {
		fmt.Printf("⚠️  Cannot open file for Telegram: %v\n", err)
		return
	}
	defer f.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", cfg.TGChatID)
	_ = mw.WriteField("caption", caption)
	_ = mw.WriteField("parse_mode", "Markdown")
	fw, err := mw.CreateFormFile("document", filepath.Base(filePath))
	if err == nil {
		io.Copy(fw, f)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", apiURL, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("⚠️  Telegram file send error: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		fmt.Printf("📤 Sent to Telegram: %s\n", filepath.Base(filePath))
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
