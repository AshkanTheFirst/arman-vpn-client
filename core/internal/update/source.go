package update

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Source identifies an archive and its signed checksum list. URLs may use
// any HTTPS host; plain HTTP is reserved for loopback test servers.
type Source struct {
	File         string `json:"file"`
	URL          string `json:"url"`
	ChecksumsURL string `json:"checksums_url"`
	SignatureURL string `json:"signature_url"`
}

const packageFile = "package.json"
const versionPrefix = "# Arman VPN version: "

func (m *Manager) defaultSource(v string) Source {
	base := m.baseURL + "v" + v + "/"
	return Source{AssetName(v), base + AssetName(v), base + SumsFile, base + SigFile}
}

func (s Source) validate() error {
	if err := validPackageName(s.File); err != nil {
		return err
	}
	for _, raw := range []string{s.URL, s.ChecksumsURL, s.SignatureURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("invalid update URL")
		}
		ip := net.ParseIP(u.Hostname())
		loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
		if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
			return fmt.Errorf("update URLs must use HTTPS")
		}
	}
	return nil
}

func validPackageName(name string) error {
	// Names are portable across the supported systems, including Windows.
	if name == "" || strings.ContainsAny(name, "/\\:\x00\r\n") || strings.Contains(name, "..") || strings.TrimSpace(name) != name {
		return fmt.Errorf("invalid update file name")
	}
	ending := ".tar.gz"
	if strings.HasSuffix(AssetName("1.0.0"), ".zip") {
		ending = ".zip"
	}
	if !strings.HasSuffix(name, ending) {
		return fmt.Errorf("the update package must be a %s archive", ending)
	}
	return nil
}

// checkedPackageHash binds a fixed filename to the requested version.
// New releases sign a version comment together with their checksums;
// earlier releases bind it through the signed versioned filename.
func checkedPackageHash(sums, sig []byte, name, v string) (string, error) {
	want, err := checkedHash(sums, sig, name)
	if err != nil {
		return "", err
	}
	var signedVersion string
	for _, line := range strings.Split(string(sums), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), versionPrefix); ok {
			if signedVersion != "" {
				return "", bad("the release's checksum list repeats its version")
			}
			signedVersion = value
		}
	}
	if signedVersion != "" {
		if signedVersion != v {
			return "", bad("the signed release version does not match %s", v)
		}
	} else if ParseSums(sums)[AssetName(v)] != want {
		return "", bad("the signed checksum list does not bind the package to version %s", v)
	}
	return want, nil
}

func (m *Manager) cachedPackageName(v string) (string, error) {
	data, err := os.ReadFile(filepath.Join(m.versionDir(v), packageFile))
	if os.IsNotExist(err) {
		return AssetName(v), nil
	}
	if err != nil {
		return "", err
	}
	var saved struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return "", err
	}
	if err := validPackageName(saved.File); err != nil {
		return "", err
	}
	return saved.File, nil
}
