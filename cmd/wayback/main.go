package main

import (
    "bufio"
    "bytes"
    "encoding/csv"
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
    URLs   []string
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
    if err != nil {
          fatalf("Cannot read domains: %v\n", err)
        }
    if len(domains) == 0 {
          fatalf("No domains provided. Use -domain or -file\n")
        }

    fmt.Printf("Scanning %d domain(s) with waybackurls...\n\n", len(domains))
    sendTelegram(cfg, fmt.Sprintf("WaybackRecon Started\n\nDomains: %d\nStarted: %s",
                                      len(domains), time.Now().UTC().Format("2006-01-02 15:04:05 UTC")), false, "")

    if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
          fatalf("Cannot create output dir: %v\n", err)
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

    for w := 0; w < workers; w++ {
          go func() {
                  for domain := range jobs {
                            urls, err := runWaybackurls(domain)
                            results <- DomainResult{Domain: domain, URLs: urls, Err: err}
                          }
                }()
        }

    for _, d := range domains {
          jobs <- d
        }
    close(jobs)

    timestamp := time.Now().UTC().Format("20060102_150405")
    var allFiles []string
    totalURLs := 0

    for i := 0; i < len(domains); i++ {
          res := <-results

          if res.Err != nil {
                  fmt.Printf("[%s] Error: %v\n", res.Domain, res.Err)
                  continue
                }

          fmt.Printf("[%s] Found %d URLs\n", res.Domain, len(res.URLs))
          totalURLs += len(res.URLs)

          if len(res.URLs) == 0 {
                  continue
                }

          filename, err := writeResults(cfg, res.Domain, res.URLs, timestamp)
          if err != nil {
                  fmt.Printf("[%s] Write error: %v\n", res.Domain, err)
                  continue
                }
          allFiles = append(allFiles, filename)
        }

    if len(domains) > 1 && totalURLs > 0 {
          allURLs := []string{}
          for _, f := range allFiles {
                  lines, _ := readLines(f)
                  allURLs = append(allURLs, lines...)
                }
          combined, err := writeResultsRaw(cfg, "combined", allURLs, timestamp)
          if err == nil {
                  allFiles = append(allFiles, combined)
                }
        }

    summary := fmt.Sprintf(
          "WaybackRecon Complete!\n\nDomains scanned: %d\nTotal URLs found: %d\nFiles: %d\nFinished: %s",
          len(domains), totalURLs, len(allFiles),
          time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
        )

    if cfg.TGToken != "" && cfg.TGChatID != "" {
          sendTelegram(cfg, summary, false, "")
          for _, f := range allFiles {
                  caption := fmt.Sprintf("File: %s", filepath.Base(f))
                  sendTelegram(cfg, caption, true, f)
                }
        } else {
          fmt.Println(summary)
        }

    fmt.Printf("\nDone! %d URLs from %d domains\n", totalURLs, len(domains))
  }

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
                  return nil, fmt.Errorf("open file: %w", err)
                }
          defer f.Close()

          scanner := bufio.NewScanner(f)
          for scanner.Scan() {
                  line := strings.TrimSpace(scanner.Text())
                  if line != "" && !strings.HasPrefix(line, "#") {
                            domains = append(domains, line)
                          }
                }
          if err := scanner.Err(); err != nil {
                  return nil, fmt.Errorf("read file: %w", err)
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

func runWaybackurls(domain string) ([]string, error) {
    fmt.Printf("Running waybackurls for: %s\n", domain)

    cmd := exec.Command("waybackurls", domain)
    out, err := cmd.Output()
    if err != nil {
          cmd2 := exec.Command("sh", "-c", fmt.Sprintf("echo '%s' | waybackurls", domain))
          out2, err2 := cmd2.Output()
          if err2 != nil {
                  return nil, fmt.Errorf("waybackurls failed: %w", err)
                }
          out = out2
        }

    var urls []string
    scanner := bufio.NewScanner(bytes.NewReader(out))
    for scanner.Scan() {
          u := strings.TrimSpace(scanner.Text())
          if u != "" {
                  urls = append(urls, u)
                }
        }

    return urls, nil
  }

func writeResults(cfg Config, domain string, urls []string, ts string) (string, error) {
    safeDomain := strings.ReplaceAll(domain, ".", "_")
    ext := cfg.OutputFmt
    if ext != "csv" && ext != "txt" {
          ext = "txt"
        }

    filename := filepath.Join(cfg.OutputDir, fmt.Sprintf("%s_%s.%s", safeDomain, ts, ext))

    f, err := os.Create(filename)
    if err != nil {
          return "", err
        }
    defer f.Close()

    if ext == "csv" {
          w := csv.NewWriter(f)
          _ = w.Write([]string{"domain", "url"})
          for _, u := range urls {
                  _ = w.Write([]string{domain, u})
                }
          w.Flush()
          return filename, w.Error()
        }

    bw := bufio.NewWriter(f)
    fmt.Fprintf(bw, "# waybackurls results for: %s\n", domain)
    fmt.Fprintf(bw, "# Generated: %s\n", time.Now().UTC().Format(time.RFC3339))
    fmt.Fprintf(bw, "# Total URLs: %d\n\n", len(urls))
    for _, u := range urls {
          fmt.Fprintln(bw, u)
        }
    return filename, bw.Flush()
  }

func writeResultsRaw(cfg Config, name string, lines []string, ts string) (string, error) {
    ext := cfg.OutputFmt
    if ext != "csv" && ext != "txt" {
          ext = "txt"
        }
    filename := filepath.Join(cfg.OutputDir, fmt.Sprintf("%s_%s.%s", name, ts, ext))
    f, err := os.Create(filename)
    if err != nil {
          return "", err
        }
    defer f.Close()
    bw := bufio.NewWriter(f)
    for _, l := range lines {
          fmt.Fprintln(bw, l)
        }
    return filename, bw.Flush()
  }

func readLines(path string) ([]string, error) {
    f, err := os.Open(path)
    if err != nil {
          return nil, err
        }
    defer f.Close()
    var lines []string
    scanner := bufio.NewScanner(f)
    for scanner.Scan() {
          lines = append(lines, scanner.Text())
        }
    return lines, scanner.Err()
  }

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
          fmt.Printf("Telegram send error: %v\n", err)
          return
        }
    defer resp.Body.Close()
  }

func sendTelegramFile(cfg Config, filePath, caption string) {
    apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendDocument", cfg.TGToken)

    f, err := os.Open(filePath)
    if err != nil {
          fmt.Printf("Cannot open file for Telegram: %v\n", err)
          return
        }
    defer f.Close()

    var buf bytes.Buffer
    mw := multipart.NewWriter(&buf)

    _ = mw.WriteField("chat_id", cfg.TGChatID)
    _ = mw.WriteField("caption", caption)

    fw, err := mw.CreateFormFile("document", filepath.Base(filePath))
    if err != nil {
          return
        }
    if _, err := io.Copy(fw, f); err != nil {
          return
        }
    mw.Close()

    req, err := http.NewRequest("POST", apiURL, &buf)
    if err != nil {
          return
        }
    req.Header.Set("Content-Type", mw.FormDataContentType())

    client := &http.Client{Timeout: 120 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
          fmt.Printf("Telegram file send error: %v\n", err)
          return
        }
    defer resp.Body.Close()

    if resp.StatusCode == 200 {
          fmt.Printf("Sent to Telegram: %s\n", filepath.Base(filePath))
        }
  }

func fatalf(format string, args ...any) {
    fmt.Fprintf(os.Stderr, format, args...)
    os.Exit(1)
  }
