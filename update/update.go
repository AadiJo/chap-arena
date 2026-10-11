// Checks GitHub for a newer release and installs it over the running executable, so the web UI can update Chap Arena
// in place. The old executable is kept beside the new one as <name>.old until the next update.

package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Where releases are published.
const ReleasesUrl = "https://api.github.com/repos/AadiJo/chap-arena/releases/latest"

// Release bundle suffix for each platform release.yml builds, keyed by GOOS/GOARCH. Keep in sync with its matrix.
var assetSuffixes = map[string]string{
	"linux/amd64":   "linux.x64",
	"darwin/amd64":  "macos.x64",
	"darwin/arm64":  "macos.m1",
	"windows/amd64": "windows.x64",
}

// Name of the executable inside a release bundle, e.g. chap-arena/chap-arena.exe on Windows.
var bundledExecutable = "chap-arena/chap-arena" + map[string]string{"windows": ".exe"}[runtime.GOOS]

// AssetName is the release bundle for this platform, e.g. chap-arena.v0.8.0.linux.x64.zip. ok is false on platforms
// release.yml doesn't build for.
func AssetName(version string) (name string, ok bool) {
	suffix, ok := assetSuffixes[runtime.GOOS+"/"+runtime.GOARCH]
	return fmt.Sprintf("chap-arena.%s.%s.zip", version, suffix), ok
}

// State of the install started by Updater.Install.
type State string

const (
	Idle        State = "idle"
	Downloading State = "downloading"
	Restarting  State = "restarting"
	Failed      State = "failed"
)

// Release is a newer version that can be installed.
type Release struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"publishedAt"`
	Size        int64     `json:"size"` // Bytes to download.

	downloadUrl string
	digest      string // e.g. "sha256:9f86d0...", as GitHub reports it.
}

// Status is included in the field status as "update".
type Status struct {
	Current string `json:"current"`
	// nil when up to date, not checked yet, offline, or running a dev build.
	Available  *Release `json:"available"`
	State      State    `json:"state"`
	Downloaded int64    `json:"downloaded"`
	Error      string   `json:"error,omitempty"` // Why the last install failed.
}

type Options struct {
	// Version this binary was built as, e.g. "v0.7.1". Anything else (a dev build) is never offered updates.
	Current string
	// GitHub's latest-release API for the repo, normally ReleasesUrl.
	ReleasesUrl string
	// Path of the running executable, symlinks resolved. It's replaced in place.
	Executable string
	// Replaces this process with the installed executable. Only returns on failure. See Restart.
	Restart func() error
}

type Updater struct {
	options Options
	client  *http.Client

	mutex      sync.Mutex
	available  *Release
	state      State
	downloaded int64
	err        string
	// The executable already holds available, so another Install only needs to restart.
	installed bool
}

func New(options Options) *Updater {
	return &Updater{options: options, client: &http.Client{Timeout: 5 * time.Minute}, state: Idle}
}

// Checks for a newer release now and then hourly, forever. Field laptops are often offline, so failures are only
// logged.
func (updater *Updater) Run() {
	if _, ok := parseVersion(updater.options.Current); !ok {
		log.Printf("Running a %s build; updates are off.", updater.options.Current)
		return
	}
	for {
		if err := updater.Check(); err != nil {
			log.Printf("Couldn't check for updates: %v", err)
		}
		time.Sleep(time.Hour)
	}
}

// Asks GitHub for the latest release and offers it if it's newer than this build and has a bundle for this platform.
func (updater *Updater) Check() error {
	current, ok := parseVersion(updater.options.Current)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, updater.options.ReleasesUrl, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := updater.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub responded %s", response.Status)
	}
	var latest struct {
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name               string `json:"name"`
			BrowserDownloadUrl string `json:"browser_download_url"`
			Size               int64  `json:"size"`
			Digest             string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(response.Body).Decode(&latest); err != nil {
		return err
	}

	var release *Release
	if version, ok := parseVersion(latest.TagName); ok && slices.Compare(version[:], current[:]) > 0 {
		name, ok := AssetName(latest.TagName)
		if !ok {
			return fmt.Errorf("%s has no build for %s/%s", latest.TagName, runtime.GOOS, runtime.GOARCH)
		}
		for _, asset := range latest.Assets {
			if asset.Name == name {
				release = &Release{
					Version: latest.TagName, PublishedAt: latest.PublishedAt, Size: asset.Size,
					downloadUrl: asset.BrowserDownloadUrl, digest: asset.Digest,
				}
			}
		}
		if release == nil {
			return fmt.Errorf("%s has no %s", latest.TagName, name)
		}
	}

	updater.mutex.Lock()
	defer updater.mutex.Unlock()
	// Leave an install alone; it's working from the release it started with.
	if updater.state == Idle || (updater.state == Failed && !updater.installed) {
		if release != nil && (updater.available == nil || updater.available.Version != release.Version) {
			log.Printf("Chap Arena %s is available (running %s).", release.Version, updater.options.Current)
		}
		updater.available = release
	}
	return nil
}

func (updater *Updater) Status() Status {
	updater.mutex.Lock()
	defer updater.mutex.Unlock()
	return Status{
		Current:    updater.options.Current,
		Available:  updater.available,
		State:      updater.state,
		Downloaded: updater.downloaded,
		Error:      updater.err,
	}
}

// Downloads the available release in the background, checks it against GitHub's sha256 digest, swaps it in for the
// executable, then restarts into it. ready is asked right before the restart and can veto it (say FMS was turned on
// during the download); the new executable stays installed and the next Install just restarts. Progress and failures
// show in Status. Errors right away if there's nothing to install or an install is already running.
func (updater *Updater) Install(ready func() error) error {
	updater.mutex.Lock()
	defer updater.mutex.Unlock()
	if updater.state == Downloading || updater.state == Restarting {
		return errors.New("an update is already in progress")
	}
	if updater.available == nil {
		return errors.New("no update is available")
	}
	release := *updater.available
	updater.state, updater.downloaded, updater.err = Downloading, 0, ""
	installed := updater.installed

	go func() {
		if !installed {
			log.Printf("Downloading Chap Arena %s", release.Version)
			if err := updater.replaceExecutable(release); err != nil {
				updater.fail(err)
				return
			}
			updater.mutex.Lock()
			updater.installed = true
			updater.mutex.Unlock()
		}
		if err := ready(); err != nil {
			updater.fail(err)
			return
		}
		updater.mutex.Lock()
		updater.state = Restarting
		updater.mutex.Unlock()
		log.Printf("Restarting into Chap Arena %s", release.Version)
		updater.fail(fmt.Errorf("restart failed: %w", updater.options.Restart()))
	}()
	return nil
}

func (updater *Updater) fail(err error) {
	log.Printf("Update failed: %v", err)
	updater.mutex.Lock()
	defer updater.mutex.Unlock()
	updater.state, updater.err = Failed, err.Error()
}

// Downloads the release bundle next to the executable, verifies it, and swaps its executable in, keeping the old one as
// <executable>.old. Windows can't overwrite a running executable but can rename it, so the swap is two renames on every
// platform. If the second fails, the first is undone.
func (updater *Updater) replaceExecutable(release Release) error {
	executable := updater.options.Executable
	bundle, err := os.CreateTemp(filepath.Dir(executable), ".update-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(bundle.Name())
	defer bundle.Close()

	response, err := updater.client.Get(release.downloadUrl)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download responded %s", response.Status)
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(bundle, hash, progressWriter{updater}), response.Body)
	if err != nil {
		return err
	}
	if digest := "sha256:" + hex.EncodeToString(hash.Sum(nil)); digest != release.digest {
		return fmt.Errorf("download doesn't match its sha256 digest (got %s, GitHub says %q)", digest, release.digest)
	}

	archive, err := zip.NewReader(bundle, size)
	if err != nil {
		return err
	}
	file, err := archive.Open(bundledExecutable)
	if err != nil {
		return fmt.Errorf("bundle has no %s", bundledExecutable)
	}
	defer file.Close()
	newPath, oldPath := executable+".new", executable+".old"
	newFile, err := os.OpenFile(newPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(newFile, file)
	if closeErr := newFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(newPath)
		return err
	}

	if err := os.Rename(executable, oldPath); err != nil {
		os.Remove(newPath)
		return err
	}
	if err := os.Rename(newPath, executable); err != nil {
		os.Rename(oldPath, executable)
		os.Remove(newPath)
		return err
	}
	log.Printf("Installed Chap Arena %s; the previous version is at %s", release.Version, oldPath)
	return nil
}

// Counts downloaded bytes into Status.
type progressWriter struct{ updater *Updater }

func (writer progressWriter) Write(p []byte) (int, error) {
	writer.updater.mutex.Lock()
	writer.updater.downloaded += int64(len(p))
	writer.updater.mutex.Unlock()
	return len(p), nil
}

// Parses a release tag like "v0.7.1". Pre-release suffixes aren't supported, since GitHub's latest release never is
// one.
func parseVersion(tag string) (version [3]int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if !strings.HasPrefix(tag, "v") || len(parts) != 3 {
		return version, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return version, false
		}
		version[i] = n
	}
	return version, true
}
