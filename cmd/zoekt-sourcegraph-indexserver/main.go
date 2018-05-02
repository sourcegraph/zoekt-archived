// zoekt-sourcegraph-indexserver periodically reindexes enabled repositories on sourcegraph
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/net/trace"

	"github.com/google/zoekt"
	"github.com/google/zoekt/build"
)

type Server struct {
	Root     *url.URL
	IndexDir string
	Interval time.Duration
	CPUCount int
}

func (s *Server) loggedRun(tr trace.Trace, cmd *exec.Cmd) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.Stdout = out
	cmd.Stderr = errOut

	tr.LazyPrintf("%s", cmd.Args)
	if err := cmd.Run(); err != nil {
		outS := out.String()
		errS := errOut.String()
		tr.LazyPrintf("failed: %v", err)
		tr.LazyPrintf("stdout: %s", outS)
		tr.LazyPrintf("stderr: %s", errS)
		tr.SetError()
		log.Printf("command %s failed: %v\nOUT: %s\nERR: %s",
			cmd.Args, err, outS, errS)
	} else {
		tr.LazyPrintf("success")
	}
}

func (s *Server) Refresh() {
	t := time.NewTicker(s.Interval)
	for {
		repos, err := listRepos(s.Root)
		if err != nil {
			log.Println(err)
			<-t.C
			continue
		}

		for _, name := range repos {
			s.index(name)
		}

		if len(repos) == 0 {
			log.Printf("no repos found")
		} else {
			// Only delete shards if we found repositories
			exists := make(map[string]bool)
			for _, name := range repos {
				exists[name] = true
			}
			s.deleteStaleIndexes(exists)
		}

		<-t.C
	}
}

func (s *Server) index(name string) {
	tr := trace.New("index", name)
	defer tr.Finish()

	commit, err := resolveRevision(s.Root, name, "HEAD")
	if err != nil || commit == "" {
		if os.IsNotExist(err) {
			// If we get to this point, it means we have an empty
			// repository (ie we know it exists). As such, we just
			// create an empty shard.
			tr.LazyPrintf("empty repository")
			s.createEmptyShard(tr, name)
			return
		}
		log.Printf("failed to resolve revision HEAD for %v: %v", name, err)
		tr.LazyPrintf("%v", err)
		return
	}

	cmd := exec.Command("zoekt-archive-index",
		fmt.Sprintf("-parallelism=%d", s.CPUCount),
		"-index", s.IndexDir,
		"-incremental",
		"-branch", "HEAD",
		"-commit", commit,
		"-name", name,
		tarballURL(s.Root, name, commit))
	// Prevent prompting
	cmd.Stdin = &bytes.Buffer{}
	s.loggedRun(tr, cmd)
}

func (s *Server) createEmptyShard(tr trace.Trace, name string) {
	cmd := exec.Command("zoekt-archive-index",
		"-index", s.IndexDir,
		"-incremental",
		"-branch", "HEAD",
		"-commit", "404aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"-name", name,
		"-")
	// Empty archive
	cmd.Stdin = bytes.NewBuffer(bytes.Repeat([]byte{0}, 1024))
	s.loggedRun(tr, cmd)
}

func (s *Server) deleteStaleIndexes(exists map[string]bool) {
	expr := s.IndexDir + "/*"
	fs, err := filepath.Glob(expr)
	if err != nil {
		log.Printf("Glob(%q): %v", expr, err)
	}

	for _, f := range fs {
		if err := deleteIfStale(exists, f); err != nil {
			log.Printf("deleteIfStale(%q): %v", f, err)
		}
	}
}

func listRepos(root *url.URL) ([]string, error) {
	u := root.ResolveReference(&url.URL{Path: "/.internal/repos/list"})
	resp, err := http.Post(u.String(), "application/json; charset=utf8", bytes.NewReader([]byte(`{"Enabled": true}`)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list repositories: status %s", resp.Status)
	}

	var data []struct {
		URI string
	}
	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		return nil, err
	}

	repos := make([]string, len(data))
	for i, r := range data {
		repos[i] = r.URI
	}
	return repos, nil
}

func resolveRevision(root *url.URL, repo, spec string) (string, error) {
	u := root.ResolveReference(&url.URL{Path: fmt.Sprintf("/.internal/git/%s/resolve-revision/%s", repo, spec)})
	resp, err := http.Get(u.String())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", os.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to resolve revision %s@%s: status %s", repo, spec, resp.Status)
	}

	var b bytes.Buffer
	_, err = b.ReadFrom(resp.Body)
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

func tarballURL(root *url.URL, repo, commit string) string {
	return root.ResolveReference(&url.URL{Path: fmt.Sprintf("/.internal/git/%s/tar/%s", repo, commit)}).String()
}

// deleteIfStale deletes the shard if its corresponding repo name is not in
// exists.
func deleteIfStale(exists map[string]bool, fn string) error {
	f, err := os.Open(fn)
	if err != nil {
		return nil
	}
	defer f.Close()

	ifile, err := zoekt.NewIndexFile(f)
	if err != nil {
		return nil
	}
	defer ifile.Close()

	repo, _, err := zoekt.ReadMetadata(ifile)
	if err != nil {
		return nil
	}

	if !exists[repo.Name] {
		log.Printf("%s no longer exists, deleting %s", repo.Name, fn)
		return os.Remove(fn)
	}

	return nil
}

func main() {
	root := flag.String("sourcegraph_url", "", "http://sourcegraph-frontend-internal or http://localhost:3090")
	interval := flag.Duration("interval", 10*time.Minute, "sync with sourcegraph this often")
	index := flag.String("index", build.DefaultDir, "set index directory to use")
	listen := flag.String("listen", "", "listen on this address.")
	cpuFraction := flag.Float64("cpu_fraction", 0.25,
		"use this fraction of the cores for indexing.")
	flag.Parse()

	if *cpuFraction <= 0.0 || *cpuFraction > 1.0 {
		log.Fatal("cpu_fraction must be between 0.0 and 1.0")
	}
	if *index == "" {
		log.Fatal("must set -index")
	}
	if *root == "" {
		log.Fatal("must set -sourcegraph_url")
	}
	rootURL, err := url.Parse(*root)
	if err != nil {
		log.Fatalf("url.Parse(%v): %v", *root, err)
	}

	// Automatically prepend our own path at the front, to minimize
	// required configuration.
	if l, err := os.Readlink("/proc/self/exe"); err == nil {
		os.Setenv("PATH", filepath.Dir(l)+":"+os.Getenv("PATH"))
	}

	if _, err := os.Stat(*index); err != nil {
		if err := os.MkdirAll(*index, 0755); err != nil {
			log.Fatalf("MkdirAll %s: %v", *index, err)
		}
	}

	cpuCount := int(math.Round(float64(runtime.NumCPU()) * (*cpuFraction)))
	if cpuCount < 1 {
		cpuCount = 1
	}
	s := &Server{
		Root:     rootURL,
		IndexDir: *index,
		Interval: *interval,
		CPUCount: cpuCount,
	}

	if *listen != "" {
		go func() {
			trace.AuthRequest = func(req *http.Request) (any, sensitive bool) {
				return true, true
			}
			log.Printf("serving HTTP on %s", *listen)
			log.Fatal(http.ListenAndServe(*listen, http.HandlerFunc(trace.Traces)))
		}()
	}

	s.Refresh()
}
