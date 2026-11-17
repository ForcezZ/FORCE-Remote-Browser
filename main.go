// FORCE Remote Browser - a file manager for the Akai Force, run from your PC.
//
// Double-click the program and a page opens in your web browser. Every file
// operation runs on the Force itself over SSH, so copying between drives
// never goes through your PC.
package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed page.html
var page string

// shq quotes a string for the POSIX shell on the Force.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ---- running commands ----

type runner interface {
	Run(cmd string) (int, string, error)
}

// localRunner runs commands on this machine (test mode, FORCE_LOCAL=1).
type localRunner struct{}

func (localRunner) Run(cmd string) (int, string, error) {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out), nil
	}
	return 0, string(out), err
}

// sshRunner uses the ssh client that ships with Windows 10/11 (and macOS/Linux).
// The command is sent to the shell on the Force through stdin, so unusual
// characters in file names cannot break it.
type sshRunner struct {
	host, user, pass string
}

func (s *sshRunner) Run(cmd string) (int, string, error) {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + os.DevNull,
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=8",
	}
	host, port, err := net.SplitHostPort(s.host)
	if err != nil {
		host = s.host
	} else {
		args = append(args, "-p", port)
	}
	env := os.Environ()
	if s.pass == "" {
		args = append(args, "-o", "BatchMode=yes")
	} else if exe, err := os.Executable(); err == nil {
		// ssh asks this program itself for the password (see main).
		env = append(env, "SSH_ASKPASS="+exe, "SSH_ASKPASS_REQUIRE=force", "DISPLAY=:0",
			"FORCE_ASKPASS_MODE=1", "FORCE_ASKPASS_PW="+s.pass)
		args = append(args, "-o", "NumberOfPasswordPrompts=1")
	}
	args = append(args, s.user+"@"+host, "sh")

	c := exec.Command("ssh", args...)
	c.Env = env
	c.Stdin = strings.NewReader(cmd + "\n")
	out, err := c.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() == 255 {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = "no response"
			}
			return 0, "", fmt.Errorf("cannot reach the Force (%s)", msg)
		}
		return ee.ExitCode(), string(out), nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("the ssh program was not found (%v). On Windows, turn on "+
			"'OpenSSH Client' under Settings > System > Optional features", err)
	}
	return 0, string(out), nil
}

func (s *sshRunner) connect() error {
	code, out, err := s.Run("echo force-ok")
	if err != nil {
		return err
	}
	if code != 0 || !strings.Contains(out, "force-ok") {
		return fmt.Errorf("unexpected response: %s", strings.TrimSpace(out))
	}
	return nil
}

// ---- settings ----

type config struct {
	Host string `json:"host"`
	User string `json:"user"`
	Root string `json:"root"`
}

func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "force_browser.json"
	}
	return filepath.Join(filepath.Dir(exe), "force_browser.json")
}

func loadConfig() config {
	c := config{User: "root", Root: "/media"}
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c config) {
	if b, err := json.MarshalIndent(c, "", "  "); err == nil {
		os.WriteFile(configPath(), b, 0o644)
	}
}

// ---- file operations ----

type app struct {
	mu  sync.Mutex
	r   runner
	cfg config
	msg string
}

type request struct {
	Op, Path, Name, Dest       string
	Sources, Paths             []string
	Host, User, Password, Root string
}

type item struct {
	Name     string `json:"name"`
	Dir      bool   `json:"dir"`
	Size     int64  `json:"size"`     // bytes, -1 if unknown
	Modified int64  `json:"modified"` // Unix seconds, 0 if unknown
}

type M map[string]any

func (a *app) state() (runner, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.r, path.Clean(a.cfg.Root)
}

// safe keeps every path inside the configured root folder.
func safe(root, p string, allowRoot bool) (string, error) {
	p = path.Clean(p)
	inside := strings.HasPrefix(p, strings.TrimRight(root, "/")+"/")
	if inside || (allowRoot && p == root) {
		return p, nil
	}
	return "", fmt.Errorf("paths outside %s are not allowed: %s", root, p)
}

func nameOK(n string) error {
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\n") {
		return fmt.Errorf("invalid name: %q", n)
	}
	return nil
}

func run(r runner, cmd string) error {
	code, out, err := r.Run(cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		out = strings.TrimSpace(out)
		if out == "" {
			out = "failed"
		}
		return errors.New(out)
	}
	return nil
}

func errList(errs []string) M {
	if errs == nil {
		errs = []string{}
	}
	return M{"errors": errs}
}

func (a *app) connect(q request) (any, error) {
	if strings.TrimSpace(q.Host) == "" {
		return nil, errors.New("enter the IP address of your Force")
	}
	root := path.Clean(q.Root)
	if !strings.HasPrefix(root, "/") || root == "/" {
		return nil, errors.New("the drives folder must be a path such as /media")
	}
	s := &sshRunner{host: q.Host, user: q.User, pass: q.Password}
	if err := s.connect(); err != nil {
		return nil, fmt.Errorf("connection failed: %v", err)
	}
	a.mu.Lock()
	a.r, a.cfg, a.msg = s, config{Host: q.Host, User: q.User, Root: root}, ""
	cfg := a.cfg
	a.mu.Unlock()
	saveConfig(cfg)
	return M{"connected": true, "host": cfg.Host, "user": cfg.User, "root": cfg.Root}, nil
}

func (a *app) handle(q request) (any, error) {
	switch q.Op {
	case "status":
		a.mu.Lock()
		defer a.mu.Unlock()
		return M{"connected": a.r != nil, "host": a.cfg.Host, "user": a.cfg.User,
			"root": a.cfg.Root, "message": a.msg}, nil
	case "connect":
		return a.connect(q)
	}

	r, root := a.state()
	if r == nil {
		return nil, errors.New("not connected to the Force")
	}

	switch q.Op {
	case "list":
		p, err := safe(root, q.Path, true)
		if err != nil {
			return nil, err
		}
		return listDir(r, p)

	case "copy", "move": // paste after Copy or Cut
		dest, err := safe(root, q.Dest, true)
		if err != nil {
			return nil, err
		}
		tool := "mv"
		if q.Op == "copy" {
			tool = "cp -r"
		}
		var errs []string
		for _, s := range q.Sources {
			err := func() error {
				src, err := safe(root, s, false)
				if err != nil {
					return err
				}
				dst := path.Join(dest, path.Base(src))
				if dst == src {
					return errors.New("it is already in this folder")
				}
				if strings.HasPrefix(dst, src+"/") {
					return errors.New("a folder cannot be pasted inside itself")
				}
				return run(r, "if [ -e "+shq(dst)+" ]; then echo 'already exists in this folder'; exit 3; fi; "+
					tool+" -- "+shq(src)+" "+shq(dst))
			}()
			if err != nil {
				errs = append(errs, path.Base(s)+": "+err.Error())
			}
		}
		return errList(errs), nil

	case "rename":
		src, err := safe(root, q.Path, false)
		if err != nil {
			return nil, err
		}
		if err := nameOK(q.Name); err != nil {
			return nil, err
		}
		dst := path.Join(path.Dir(src), q.Name)
		if err := run(r, "if [ -e "+shq(dst)+" ]; then echo 'that name already exists'; exit 3; fi; mv -- "+
			shq(src)+" "+shq(dst)); err != nil {
			return errList([]string{err.Error()}), nil
		}
		return errList(nil), nil

	case "mkdir":
		p, err := safe(root, q.Path, true)
		if err != nil {
			return nil, err
		}
		if err := nameOK(q.Name); err != nil {
			return nil, err
		}
		if err := run(r, "mkdir -- "+shq(path.Join(p, q.Name))); err != nil {
			return errList([]string{err.Error()}), nil
		}
		return errList(nil), nil

	case "delete":
		var errs []string
		for _, s := range q.Paths {
			p, err := safe(root, s, false)
			if err == nil {
				err = run(r, "rm -rf -- "+shq(p))
			}
			if err != nil {
				errs = append(errs, path.Base(s)+": "+err.Error())
			}
		}
		return errList(errs), nil
	}
	return nil, errors.New("unknown command")
}

// listDir lists a folder with size and modification time. It uses stat,
// which the Force's BusyBox provides, and falls back to plain ls if needed.
// -L follows links, so a link to a folder shows as a folder.
func listDir(r runner, p string) (any, error) {
	cmd := "cd -- " + shq(p) + " || exit 2\n" +
		"if command -v stat >/dev/null 2>&1; then\n" +
		"  for f in * .[!.]* ..?*; do [ -e \"$f\" ] || [ -L \"$f\" ] || continue\n" +
		"    stat -L -c '%F|%s|%Y|%n' -- \"$f\" 2>/dev/null || { [ -d \"$f\" ] && echo \"directory|-1|0|$f\" || echo \"file|-1|0|$f\"; }; done\n" +
		"else echo NOSTAT; ls -1Ap; fi"
	code, out, err := r.Run(cmd)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = "cannot open this folder"
		}
		return nil, errors.New(msg)
	}
	items := []item{}
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	if len(lines) > 0 && lines[0] == "NOSTAT" {
		for _, n := range lines[1:] {
			if n != "" {
				items = append(items, item{Name: strings.TrimSuffix(n, "/"), Dir: strings.HasSuffix(n, "/"), Size: -1})
			}
		}
	} else {
		for _, l := range lines {
			f := strings.SplitN(l, "|", 4)
			if len(f) != 4 || f[3] == "" {
				continue
			}
			it := item{Name: f[3], Dir: f[0] == "directory", Size: -1}
			if !it.Dir {
				fmt.Sscan(f[1], &it.Size)
			}
			fmt.Sscan(f[2], &it.Modified)
			items = append(items, it)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Dir != items[j].Dir {
			return items[i].Dir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return M{"path": p, "items": items}, nil
}

// ---- web server ----

func openBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

func main() {
	if os.Getenv("FORCE_ASKPASS_MODE") == "1" { // called by ssh to get the password
		fmt.Println(os.Getenv("FORCE_ASKPASS_PW"))
		return
	}
	a := &app{cfg: loadConfig()}
	if os.Getenv("FORCE_LOCAL") != "" {
		a.r = localRunner{}
		a.cfg.Host = "test mode"
		if v := os.Getenv("FORCE_ROOT"); v != "" {
			a.cfg.Root = v
		}
	} else if a.cfg.Host != "" { // used before: connect right away
		s := &sshRunner{host: a.cfg.Host, user: a.cfg.User}
		if err := s.connect(); err == nil {
			a.r = s
		} else {
			a.msg = "Could not connect to " + a.cfg.Host + " automatically: " + err.Error()
		}
	}

	tb := make([]byte, 16)
	rand.Read(tb)
	token := hex.EncodeToString(tb)
	html := strings.Replace(page, "__TOKEN__", token, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(html))
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != "POST" || r.Header.Get("X-Token") != token {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(M{"error": "forbidden"})
			return
		}
		var q request
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			json.NewEncoder(w).Encode(M{"error": "invalid request"})
			return
		}
		res, err := a.handle(q)
		if err != nil {
			res = M{"error": err.Error()}
		}
		json.NewEncoder(w).Encode(res)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:8765")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Println("Cannot start:", err)
			time.Sleep(8 * time.Second)
			return
		}
	}
	url := "http://" + ln.Addr().String() + "/"
	fmt.Println("FORCE Remote Browser is running.")
	fmt.Println("The page opens in your web browser:", url)
	fmt.Println()
	fmt.Println("Keep this window open while you use it. Close it to stop.")
	if os.Getenv("FORCE_NOBROWSER") == "" {
		openBrowser(url)
	}
	http.Serve(ln, mux)
}
