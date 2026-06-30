package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	MANIFEST_URL     = "https://raw.githubusercontent.com/vaibhavkumar-del/gnss-sender/main/manifest.json"
	GH_RAW_HOST      = "raw.githubusercontent.com"
	GH_RAW_IP        = "185.199.108.133" // GitHub CDN — whitelisted by Airtel, stable /22 range
	CHECK_INTERVAL   = 30 * time.Minute
	NAND_MOUNT       = "/data/nand"
	NAND_VER_DIR     = "/data/nand/fota"
	FALLBACK_VER_DIR = "/data/fota"
)

type Program struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	BinaryPath string `json:"binary_path"`
	Service    string `json:"service"`
	URL        string `json:"url"`
}

type Manifest struct {
	Updated  string    `json:"updated"`
	Programs []Program `json:"programs"`
}

func isNANDMounted() bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == NAND_MOUNT {
			return true
		}
	}
	return false
}

// readLocalVersion checks NAND first (if mounted), then the fallback path.
// Handles the startup window where the agent runs before NAND mounts.
func readLocalVersion(name string) string {
	if isNANDMounted() {
		if data, err := os.ReadFile(NAND_VER_DIR + "/" + name + ".ver"); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	if data, err := os.ReadFile(FALLBACK_VER_DIR + "/" + name + ".ver"); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func writeLocalVersion(name, version string) error {
	dir := FALLBACK_VER_DIR
	if isNANDMounted() {
		dir = NAND_VER_DIR
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(dir+"/"+name+".ver", []byte(version), 0644)
}

func fetchManifest(client *http.Client) (*Manifest, error) {
	resp, err := client.Get(MANIFEST_URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching manifest", resp.StatusCode)
	}
	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

func downloadBinary(client *http.Client, url, destPath string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d downloading binary", resp.StatusCode)
	}

	tmpPath := destPath + ".fota_tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("open tmp file: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write tmp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tmp file: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomic replace: %w", err)
	}
	return nil
}

func restartService(service string) error {
	out, err := exec.Command("systemctl", "restart", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %v — %s", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func checkAndUpdate(client *http.Client) {
	fmt.Println("[FOTA] Fetching manifest...")
	manifest, err := fetchManifest(client)
	if err != nil {
		fmt.Printf("[FOTA] Manifest fetch failed: %v\n", err)
		return
	}
	fmt.Printf("[FOTA] Manifest date: %s | programs: %d\n", manifest.Updated, len(manifest.Programs))

	for _, prog := range manifest.Programs {
		local := readLocalVersion(prog.Name)
		fmt.Printf("[FOTA] %-20s local=%-8q remote=%q\n", prog.Name, local, prog.Version)

		if local == prog.Version {
			fmt.Printf("[FOTA] %s is up to date (%s)\n", prog.Name, prog.Version)
			continue
		}

		fmt.Printf("[FOTA] Updating %s: %s → %s\n", prog.Name, local, prog.Version)
		fmt.Printf("[FOTA] Downloading from %s\n", prog.URL)

		if err := downloadBinary(client, prog.URL, prog.BinaryPath); err != nil {
			fmt.Printf("[FOTA] Download failed for %s: %v\n", prog.Name, err)
			continue
		}
		fmt.Printf("[FOTA] Binary replaced at %s\n", prog.BinaryPath)

		if err := writeLocalVersion(prog.Name, prog.Version); err != nil {
			fmt.Printf("[FOTA] Version write failed for %s: %v\n", prog.Name, err)
		}

		fmt.Printf("[FOTA] Restarting %s...\n", prog.Service)
		if err := restartService(prog.Service); err != nil {
			fmt.Printf("[FOTA] Service restart failed: %v\n", err)
		} else {
			fmt.Printf("[FOTA] %s updated to %s and restarted\n", prog.Name, prog.Version)
		}
	}
}

func main() {
	fmt.Println("[FOTA] Central FOTA agent starting")
	fmt.Printf("[FOTA] Manifest URL: %s\n", MANIFEST_URL)
	fmt.Printf("[FOTA] Check interval: %s\n", CHECK_INTERVAL)

	// Connect to GitHub CDN IP directly with no SNI in the TLS ClientHello.
	// Carrier DPI filters by SNI; omitting it (ServerName="") bypasses the filter.
	// GitHub's TLS stack serves a default cert which we skip-verify.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         "", // no SNI — carrier cannot filter what it cannot see
			},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if host == GH_RAW_HOST {
					addr = GH_RAW_IP + ":443"
				}
				return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, addr)
			},
		},
	}

	checkAndUpdate(client)

	ticker := time.NewTicker(CHECK_INTERVAL)
	defer ticker.Stop()

	for range ticker.C {
		checkAndUpdate(client)
	}
}
