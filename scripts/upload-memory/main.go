// Command upload-memory measures the memory of the real server under
// concurrent worst-case uploads (docs/measurements/upload-memory.md). It is a
// local measurement tool, not part of the application; point it only at a
// disposable database schema.
//
//	LEARNOO_WORST_CASE_DIR=/tmp/in go test -count=1 -run WriteWorstCaseInputs ./internal/media
//	go build -o /tmp/server ./cmd/server
//	go run ./scripts/upload-memory -server /tmp/server -inputs /tmp/in \
//	    -db 'postgres://…/learnoo_test?sslmode=disable&search_path=memmeasure'
//
// It migrates the schema and creates a throwaway author with a random
// password (with the -server binary), then keeps -uploaders uploads of the
// inputs in flight plus -readers clients fetching stored media and /healthz.
// By default it starts `server serve` itself and samples its RSS with ps(1)
// (and, with -gctrace, the collector's heap sizes). With -url it uses a
// server that is already running, for example in a container, and with
// -container it samples that container's memory with `docker stats`.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type options struct {
	server, inputs, db, only string
	url, container           string
	gomemlimit, gogc         string
	stderr                   string
	gctrace                  bool
	uploaders, readers       int
	duration                 time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.server, "server", "", "server binary for migrate and create-user, and to start unless -url (required)")
	flag.StringVar(&o.inputs, "inputs", "", "directory of inputs written by TestWriteWorstCaseInputs (required)")
	flag.StringVar(&o.db, "db", "", "database URL of a disposable schema (required)")
	flag.StringVar(&o.only, "only", "", "comma-separated input file names (default: all)")
	flag.StringVar(&o.url, "url", "", "base URL of a running server using the same database (default: start one)")
	flag.StringVar(&o.container, "container", "", "with -url: container whose memory to sample with docker stats")
	flag.StringVar(&o.gomemlimit, "gomemlimit", "", "GOMEMLIMIT for a started server (empty: unset)")
	flag.StringVar(&o.gogc, "gogc", "", "GOGC for a started server (empty: unset)")
	flag.StringVar(&o.stderr, "stderr", "", "file for a started server's standard error")
	flag.BoolVar(&o.gctrace, "gctrace", false, "run a started server with GODEBUG=gctrace=1 and report its heap sizes")
	flag.IntVar(&o.uploaders, "uploaders", 4, "concurrent uploads (studioapi.MaxConcurrentUploads)")
	flag.IntVar(&o.readers, "readers", 2, "concurrent readers")
	flag.DurationVar(&o.duration, "duration", 60*time.Second, "how long to keep uploading")
	flag.Parse()
	if o.server == "" || o.inputs == "" || o.db == "" || (o.gctrace && o.stderr == "") {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "upload-memory:", err)
		os.Exit(1)
	}
}

type input struct {
	name, contentType string
	data              []byte
}

func loadInputs(dir, only string) ([]input, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range strings.Split(only, ",") {
		if n != "" {
			want[n] = true
		}
	}
	var out []input
	for _, p := range names {
		name := filepath.Base(p)
		if len(want) > 0 && !want[name] {
			continue
		}
		ct := map[string]string{".jpg": "image/jpeg", ".png": "image/png", ".webp": "image/webp"}[filepath.Ext(name)]
		if ct == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, input{name, ct, b})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no inputs in %s", dir)
	}
	return out, nil
}

func command(server, db, stdin string, args ...string) error {
	cmd := exec.Command(server, args...)
	cmd.Env = append(os.Environ(), "LEARNOO_DATABASE_URL="+db)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// sampler records memory samples in bytes.
type sampler struct {
	mu      sync.Mutex
	samples []int64
}

func (s *sampler) add(v int64) {
	s.mu.Lock()
	s.samples = append(s.samples, v)
	s.mu.Unlock()
}

func (s *sampler) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) == 0 {
		return "no samples"
	}
	v := append([]int64(nil), s.samples...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return fmt.Sprintf("peak %.0f MiB, p95 %.0f MiB, median %.0f MiB (%d samples)",
		mib(v[len(v)-1]), mib(v[len(v)*95/100]), mib(v[len(v)/2]), len(v))
}

func mib(v int64) float64 { return float64(v) / (1 << 20) }

func rss(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return kb << 10, err
}

// sampleProcess samples the RSS of pid every 50 ms until ctx ends.
func sampleProcess(ctx context.Context, pid int, s *sampler) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if v, err := rss(pid); err == nil {
				s.add(v)
			}
		}
	}
}

// parseSize reads docker's "123.4MiB" notation.
func parseSize(s string) (int64, bool) {
	units := []struct {
		suffix string
		mult   float64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			f, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			return int64(f * u.mult), err == nil
		}
	}
	return 0, false
}

// sampleContainer streams `docker stats` (about one sample per second).
func sampleContainer(ctx context.Context, name string, s *sampler) {
	cmd := exec.CommandContext(ctx, "docker", "stats", "--format", "{{.MemUsage}}", name)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.LastIndex(line, "\x1b[H"); i >= 0 { // screen clearing
			line = line[i+3:]
		}
		used, _, _ := strings.Cut(line, " / ")
		if v, ok := parseSize(strings.TrimSpace(used)); ok {
			s.add(v)
		}
	}
	cmd.Wait()
}

// gcHeap returns the largest heap at the start of a collection and the
// largest heap left live after one, from GODEBUG=gctrace=1 output.
func gcHeap(file string) (start, live int64) {
	f, err := os.Open(file)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		for i, fld := range fields {
			if i+1 < len(fields) && fields[i+1] == "MB," && strings.Count(fld, "->") == 2 {
				p := strings.Split(fld, "->")
				a, _ := strconv.ParseInt(p[0], 10, 64)
				c, _ := strconv.ParseInt(p[2], 10, 64)
				start, live = max(start, a<<20), max(live, c<<20)
			}
		}
	}
	return start, live
}

func run(o options) error {
	ins, err := loadInputs(o.inputs, o.only)
	if err != nil {
		return err
	}
	if err := command(o.server, o.db, "", "migrate"); err != nil {
		return err
	}
	pw := make([]byte, 16)
	rand.Read(pw)
	password := hex.EncodeToString(pw)
	email := fmt.Sprintf("measure-%d@example.test", time.Now().UnixNano())
	if err := command(o.server, o.db, password+"\n", "create-user", "-email", email, "-name", "Measurement"); err != nil {
		return err
	}
	if err := command(o.server, o.db, "", "grant", "-email", email, "-permission", "author"); err != nil {
		return err
	}

	base := o.url
	var env []string
	var cmd *exec.Cmd
	if base == "" {
		addr, err := freePort()
		if err != nil {
			return err
		}
		media, err := os.MkdirTemp("", "learnoo-measure-media-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(media)
		if o.gomemlimit != "" {
			env = append(env, "GOMEMLIMIT="+o.gomemlimit)
		}
		if o.gogc != "" {
			env = append(env, "GOGC="+o.gogc)
		}
		if o.gctrace {
			env = append(env, "GODEBUG=gctrace=1")
		}
		cmd = exec.Command(o.server, "serve")
		cmd.Env = append(os.Environ(), "LEARNOO_DATABASE_URL="+o.db, "LEARNOO_ADDR="+addr, "LEARNOO_MEDIA_DIR="+media)
		cmd.Env = append(cmd.Env, env...)
		cmd.Stderr = io.Discard
		if o.stderr != "" {
			f, err := os.Create(o.stderr)
			if err != nil {
				return err
			}
			defer f.Close()
			cmd.Stderr = f
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		defer func() {
			cmd.Process.Signal(os.Interrupt)
			cmd.Wait()
		}()
		base = "http://" + addr
	}
	client := &http.Client{Timeout: 60 * time.Second}
	for i := 0; ; i++ {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if i > 100 {
			return fmt.Errorf("server did not start: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := client.Post(base+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	var session struct {
		CSRF string `json:"csrfToken"`
	}
	err = json.NewDecoder(resp.Body).Decode(&session)
	resp.Body.Close()
	var cookie string
	for _, c := range resp.Cookies() {
		cookie = c.Name + "=" + c.Value
	}
	if err != nil || resp.StatusCode != 200 || cookie == "" {
		return fmt.Errorf("login: %d %v", resp.StatusCode, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), o.duration)
	defer cancel()
	var mem sampler
	var idle int64
	samplerDone := make(chan struct{})
	switch {
	case cmd != nil:
		idle, _ = rss(cmd.Process.Pid)
		go func() { defer close(samplerDone); sampleProcess(ctx, cmd.Process.Pid, &mem) }()
	case o.container != "":
		go func() { defer close(samplerDone); sampleContainer(ctx, o.container, &mem) }()
	default:
		close(samplerDone)
	}

	var (
		mu       sync.Mutex
		statuses = map[string]int{}
		urls     []string
		uploads  atomic.Int64
		reads    atomic.Int64
		failed   time.Duration // first transport error, after the start
	)
	start := time.Now()
	var wg sync.WaitGroup
	for u := 0; u < o.uploaders; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			for i := u; ctx.Err() == nil; i++ {
				in := ins[i%len(ins)]
				req, _ := http.NewRequest("POST", base+"/api/media", bytes.NewReader(in.data))
				req.Header.Set("Content-Type", in.contentType)
				req.Header.Set("Cookie", cookie)
				req.Header.Set("X-CSRF-Token", session.CSRF)
				resp, err := client.Do(req)
				key := in.name + " "
				if err != nil {
					key += "error"
					mu.Lock()
					if failed == 0 {
						failed = time.Since(start)
					}
					mu.Unlock()
					time.Sleep(100 * time.Millisecond) // the server may be gone
				} else {
					var r struct {
						URL   string `json:"url"`
						Error struct{ Code string }
					}
					json.NewDecoder(resp.Body).Decode(&r)
					resp.Body.Close()
					key += strconv.Itoa(resp.StatusCode) + " " + r.Error.Code
					if r.URL != "" {
						mu.Lock()
						urls = append(urls, r.URL)
						mu.Unlock()
					}
				}
				uploads.Add(1)
				mu.Lock()
				statuses[key]++
				mu.Unlock()
			}
		}(u)
	}
	for r := 0; r < o.readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := r; ctx.Err() == nil; i++ {
				url := base + "/healthz"
				mu.Lock()
				if len(urls) > 0 && i%2 == 0 {
					url = base + urls[i%len(urls)]
				}
				mu.Unlock()
				req, _ := http.NewRequest("GET", url, nil)
				req.Header.Set("Cookie", cookie)
				resp, err := client.Do(req)
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				reads.Add(1)
			}
		}(r)
	}
	wg.Wait()
	<-samplerDone

	fmt.Printf("env %v, uploaders %d, readers %d, duration %s, inputs %d\n", env, o.uploaders, o.readers, o.duration, len(ins))
	switch {
	case cmd != nil:
		fmt.Printf("server RSS: idle %.0f MiB, %s\n", mib(idle), mem.summary())
	case o.container != "":
		fmt.Printf("container memory (docker stats): %s\n", mem.summary())
	}
	if o.gctrace {
		start, live := gcHeap(o.stderr)
		fmt.Printf("Go heap (gctrace): largest at GC start %.0f MiB, largest live after GC %.0f MiB\n", mib(start), mib(live))
	}
	fmt.Printf("uploads %d, reads %d\n", uploads.Load(), reads.Load())
	if failed > 0 {
		fmt.Printf("first transport error after %s\n", failed.Round(100*time.Millisecond))
	}
	keys := make([]string, 0, len(statuses))
	for k := range statuses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s: %d\n", k, statuses[k])
	}
	return nil
}
