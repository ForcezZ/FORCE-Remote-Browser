// FORCE Remote Browser - a file manager for the Akai Force, run from your PC.
//
// Double-click the program and a page opens in your web browser: the Force's
// drives on the left, this computer's drives on the right. Work on the Force
// runs on the Force itself over SSH, so copying between its drives never goes
// through your PC; files move between the PC and the Force as a tar stream.
package main

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// runner runs a shell command on the Force. stdin, when set, is sent to the
// command; stdout, when set, receives its output and only errors come back as
// text. Without stdout, output and errors come back together.
type runner interface {
	Exec(cmd string, stdin io.Reader, stdout io.Writer) (int, string, error)
}

func runOut(r runner, cmd string) (int, string, error) { return r.Exec(cmd, nil, nil) }

// localRunner runs commands on this machine (test mode, FORCE_LOCAL=1).
type localRunner struct{}

func (localRunner) Exec(cmd string, stdin io.Reader, stdout io.Writer) (int, string, error) {
	c := exec.Command("sh", "-c", cmd)
	c.Stdin = stdin
	var buf bytes.Buffer
	c.Stderr = &buf
	if stdout != nil {
		c.Stdout = stdout
	} else {
		c.Stdout = &buf
	}
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), buf.String(), nil
	}
	return 0, buf.String(), err
}

// sshRunner uses the ssh client that ships with Windows 10/11 (and macOS/Linux).
// Normal commands are sent to the shell on the Force through stdin, so unusual
// characters in file names cannot break them. Transfers send file data
// through stdin instead, with a one-line command.
type sshRunner struct {
	host, user, pass string
}

func (s *sshRunner) Exec(cmd string, stdin io.Reader, stdout io.Writer) (int, string, error) {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + os.DevNull,
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=8",
		"-o", "ServerAliveInterval=15",
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
	if stdin == nil {
		args = append(args, s.user+"@"+host, "sh")
		stdin = strings.NewReader(cmd + "\n")
	} else {
		args = append(args, s.user+"@"+host, cmd)
	}

	c := exec.Command("ssh", args...)
	c.Env = env
	c.Stdin = stdin
	var buf bytes.Buffer
	c.Stderr = &buf
	if stdout != nil {
		c.Stdout = stdout
	} else {
		c.Stdout = &buf
	}
	err = c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() == 255 {
			msg := strings.TrimSpace(buf.String())
			if msg == "" {
				msg = "no response"
			}
			return 0, "", fmt.Errorf("cannot reach the Force (%s)", msg)
		}
		return ee.ExitCode(), buf.String(), nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("the ssh program was not found (%v). On Windows, turn on "+
			"'OpenSSH Client' under Settings > System > Optional features", err)
	}
	return 0, buf.String(), nil
}

func (s *sshRunner) connect() error {
	code, out, err := runOut(s, "echo force-ok")
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
//
// There are two sides: "force" (files on the Force, reached over SSH) and
// "pc" (files on this computer). Paths on both sides use forward slashes.
// On Windows the PC side's top level "/" lists the drives, and "/C:/Users"
// means C:\Users.

type app struct {
	mu  sync.Mutex
	r   runner
	cfg config
	msg string
}

type request struct {
	Op, Path, Name, Dest       string
	Side, SrcSide, DestSide    string
	Sources, Paths             []string
	Host, User, Password, Root string
}

type item struct {
	Name     string `json:"name"`
	Dir      bool   `json:"dir"`
	Drive    bool   `json:"drive,omitempty"`
	Size     int64  `json:"size"`     // bytes, -1 if unknown
	Modified int64  `json:"modified"` // Unix seconds, 0 if unknown
}

type M map[string]any

func (a *app) state() (runner, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.r, path.Clean(a.cfg.Root)
}

// safe keeps every Force path inside the configured root folder.
func safe(root, p string, allowRoot bool) (string, error) {
	p = path.Clean(p)
	inside := strings.HasPrefix(p, strings.TrimRight(root, "/")+"/")
	if inside || (allowRoot && p == root) {
		return p, nil
	}
	return "", fmt.Errorf("paths outside %s are not allowed: %s", root, p)
}

const windows = runtime.GOOS == "windows"

// pcSafe checks a PC path. The top level and (on Windows) whole drives can be
// listed and pasted into, but never renamed or deleted.
func pcSafe(p string, allowRoot bool) (string, error) {
	p = path.Clean("/" + strings.TrimLeft(p, "/"))
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	isRoot := p == "/"
	if windows && !isRoot {
		d := parts[0]
		if len(d) != 2 || d[1] != ':' || !((d[0] >= 'A' && d[0] <= 'Z') || (d[0] >= 'a' && d[0] <= 'z')) {
			return "", fmt.Errorf("not a drive path: %s", p)
		}
		isRoot = len(parts) == 1
	}
	if isRoot && !allowRoot {
		return "", errors.New("a whole drive cannot be changed")
	}
	if windows && p == "/" && !allowRoot {
		return "", errors.New("not allowed here")
	}
	return p, nil
}

// osPath turns a PC path into the operating system's own form.
func osPath(p string) string {
	if !windows {
		return p
	}
	s := strings.TrimPrefix(p, "/")
	if len(s) == 2 {
		return s + `\`
	}
	return filepath.FromSlash(s)
}

func nameOK(n string) error {
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\n") || (windows && strings.ContainsAny(n, `\:*?"<>|`)) {
		return fmt.Errorf("invalid name: %q", n)
	}
	return nil
}

func run(r runner, cmd string) error {
	code, out, err := runOut(r, cmd)
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

func remoteExists(r runner, p string) (bool, error) {
	code, out, err := runOut(r, "if [ -e "+shq(p)+" ] || [ -L "+shq(p)+" ]; then echo yes; else echo no; fi")
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, errors.New(strings.TrimSpace(out))
	}
	return strings.Contains(out, "yes"), nil
}

func localExists(p string) bool {
	_, err := os.Lstat(osPath(p))
	return err == nil
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

func sideOf(s string) string {
	if s == "pc" {
		return "pc"
	}
	return "force"
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
	needForce := sideOf(q.Side) == "force"
	if q.Op == "copy" || q.Op == "move" {
		needForce = sideOf(q.SrcSide) == "force" || sideOf(q.DestSide) == "force"
	}
	if needForce && r == nil {
		return nil, errors.New("not connected to the Force")
	}
	pc := sideOf(q.Side) == "pc"

	switch q.Op {
	case "list":
		if pc {
			p, err := pcSafe(q.Path, true)
			if err != nil {
				return nil, err
			}
			return localList(p)
		}
		p, err := safe(root, q.Path, true)
		if err != nil {
			return nil, err
		}
		return listDir(r, p)

	case "copy", "move": // paste after Copy or Cut
		return a.paste(r, root, q), nil

	case "rename":
		if err := nameOK(q.Name); err != nil {
			return nil, err
		}
		if pc {
			src, err := pcSafe(q.Path, false)
			if err != nil {
				return nil, err
			}
			dst := path.Join(path.Dir(src), q.Name)
			if localExists(dst) {
				return errList([]string{"that name already exists"}), nil
			}
			if err := os.Rename(osPath(src), osPath(dst)); err != nil {
				return errList([]string{err.Error()}), nil
			}
			return errList(nil), nil
		}
		src, err := safe(root, q.Path, false)
		if err != nil {
			return nil, err
		}
		dst := path.Join(path.Dir(src), q.Name)
		if err := run(r, "if [ -e "+shq(dst)+" ]; then echo 'that name already exists'; exit 3; fi; mv -- "+
			shq(src)+" "+shq(dst)); err != nil {
			return errList([]string{err.Error()}), nil
		}
		return errList(nil), nil

	case "mkdir":
		if err := nameOK(q.Name); err != nil {
			return nil, err
		}
		if pc {
			p, err := pcSafe(q.Path, true)
			if err != nil {
				return nil, err
			}
			if windows && p == "/" {
				return nil, errors.New("open a drive first")
			}
			if err := os.Mkdir(osPath(path.Join(p, q.Name)), 0o755); err != nil {
				return errList([]string{err.Error()}), nil
			}
			return errList(nil), nil
		}
		p, err := safe(root, q.Path, true)
		if err != nil {
			return nil, err
		}
		if err := run(r, "mkdir -- "+shq(path.Join(p, q.Name))); err != nil {
			return errList([]string{err.Error()}), nil
		}
		return errList(nil), nil

	case "delete":
		var errs []string
		for _, s := range q.Paths {
			var err error
			if pc {
				var p string
				if p, err = pcSafe(s, false); err == nil {
					err = os.RemoveAll(osPath(p))
				}
			} else {
				var p string
				if p, err = safe(root, s, false); err == nil {
					err = run(r, "rm -rf -- "+shq(p))
				}
			}
			if err != nil {
				errs = append(errs, path.Base(s)+": "+err.Error())
			}
		}
		return errList(errs), nil
	}
	return nil, errors.New("unknown command")
}

// paste copies or moves items, on one side or between the Force and the PC.
func (a *app) paste(r runner, root string, q request) M {
	from, to := sideOf(q.SrcSide), sideOf(q.DestSide)
	move := q.Op == "move"
	var dest string
	var err error
	if to == "pc" {
		dest, err = pcSafe(q.Dest, true)
		if err == nil && windows && dest == "/" {
			err = errors.New("open a drive first")
		}
	} else {
		dest, err = safe(root, q.Dest, true)
	}
	if err != nil {
		return errList([]string{err.Error()})
	}
	var errs []string
	for _, s := range q.Sources {
		err := func() error {
			var src string
			var err error
			if from == "pc" {
				src, err = pcSafe(s, false)
			} else {
				src, err = safe(root, s, false)
			}
			if err != nil {
				return err
			}
			dst := path.Join(dest, path.Base(src))
			if from == to {
				if dst == src {
					return errors.New("it is already in this folder")
				}
				if strings.HasPrefix(dst, src+"/") {
					return errors.New("a folder cannot be pasted inside itself")
				}
			}
			switch {
			case from == "force" && to == "force":
				tool := "cp -r"
				if move {
					tool = "mv"
				}
				return run(r, "if [ -e "+shq(dst)+" ]; then echo 'already exists in this folder'; exit 3; fi; "+
					tool+" -- "+shq(src)+" "+shq(dst))

			case from == "pc" && to == "pc":
				if localExists(dst) {
					return errors.New("already exists in this folder")
				}
				if move && os.Rename(osPath(src), osPath(dst)) == nil {
					return nil
				}
				if err := localCopy(osPath(src), osPath(dst)); err != nil {
					return err
				}
				if move {
					return os.RemoveAll(osPath(src))
				}
				return nil

			case from == "pc" && to == "force":
				ex, err := remoteExists(r, dst)
				if err != nil {
					return err
				}
				if ex {
					return errors.New("already exists in this folder")
				}
				if err := upload(r, osPath(src), path.Base(src), dest); err != nil {
					return err
				}
				if move {
					return os.RemoveAll(osPath(src))
				}
				return nil

			default: // force to pc
				if localExists(dst) {
					return errors.New("already exists in this folder")
				}
				if err := download(r, src, osPath(dest)); err != nil {
					return err
				}
				if move {
					return run(r, "rm -rf -- "+shq(src))
				}
				return nil
			}
		}()
		if err != nil {
			errs = append(errs, path.Base(s)+": "+err.Error())
		}
	}
	return errList(errs)
}

// ---- PC side ----

func localList(p string) (any, error) {
	items := []item{}
	if windows && p == "/" {
		for c := 'A'; c <= 'Z'; c++ {
			if _, err := os.Stat(string(c) + `:\`); err == nil {
				items = append(items, item{Name: string(c) + ":", Dir: true, Drive: true, Size: -1})
			}
		}
		return M{"path": p, "items": items}, nil
	}
	dir := osPath(p)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot open this folder: %v", err)
	}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(dir, e.Name())) // follows links
		if err != nil {
			if info, err = e.Info(); err != nil {
				continue
			}
		}
		it := item{Name: e.Name(), Dir: info.IsDir(), Size: -1, Modified: info.ModTime().Unix()}
		if !it.Dir {
			it.Size = info.Size()
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Dir != items[j].Dir {
			return items[i].Dir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return M{"path": p, "items": items}, nil
}

// localCopy copies a file or folder on the PC. It never overwrites.
func localCopy(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.Mkdir(dst, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := localCopy(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// ---- transfers between the PC and the Force (a tar stream over ssh) ----

// upload sends a PC file or folder into the Force folder dest.
func upload(r runner, src, base, dest string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, src, base)) }()
	code, out, err := r.Exec("cd -- "+shq(dest)+" && tar -xf -", pr, io.Discard)
	pr.CloseWithError(errors.New("transfer stopped"))
	if err != nil {
		return err
	}
	if code != 0 {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = "upload failed"
		}
		return errors.New(msg)
	}
	return nil
}

func writeTar(w io.Writer, src, base string) error {
	tw := tar.NewWriter(w)
	err := filepath.Walk(src, func(fp string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil // skip links and special files
		}
		rel, err := filepath.Rel(src, fp)
		if err != nil {
			return err
		}
		name := base
		if rel != "." {
			name = base + "/" + filepath.ToSlash(rel)
		}
		hdr := &tar.Header{Name: name, ModTime: info.ModTime(), Format: tar.FormatGNU}
		if info.IsDir() {
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, name+"/", 0o755
			return tw.WriteHeader(hdr)
		}
		hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, 0o644, info.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(fp)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(tw, f, info.Size())
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// download fetches a Force file or folder into the PC folder destDir.
func download(r runner, src, destDir string) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := readTar(pr, destDir)
		if err == nil {
			io.Copy(io.Discard, pr) // tar pads the end of the archive
		}
		pr.CloseWithError(errors.New("transfer stopped"))
		done <- err
	}()
	code, out, err := r.Exec("cd -- "+shq(path.Dir(src))+" && tar -cf - "+shq("./"+path.Base(src)), nil, pw)
	pw.Close()
	xerr := <-done
	if err != nil {
		return err
	}
	if code != 0 {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = "download failed"
		}
		return errors.New(msg)
	}
	return xerr
}

func readTar(rd io.Reader, destDir string) error {
	tr := tar.NewReader(rd)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/") // cannot climb out of destDir
		if name == "" {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
	}
}

// listDir lists a Force folder with size and modification time. It uses stat,
// which the Force's BusyBox provides, and falls back to plain ls if needed.
// -L follows links, so a link to a folder shows as a folder.
func listDir(r runner, p string) (any, error) {
	cmd := "cd -- " + shq(p) + " || exit 2\n" +
		"if command -v stat >/dev/null 2>&1; then\n" +
		"  for f in * .[!.]* ..?*; do [ -e \"$f\" ] || [ -L \"$f\" ] || continue\n" +
		"    stat -L -c '%F|%s|%Y|%n' -- \"$f\" 2>/dev/null || { [ -d \"$f\" ] && echo \"directory|-1|0|$f\" || echo \"file|-1|0|$f\"; }; done\n" +
		"else echo NOSTAT; ls -1Ap; fi"
	code, out, err := runOut(r, cmd)
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
