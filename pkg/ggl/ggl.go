package ggl

import (
	"github.com/xanzy/go-gitlab"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
)

// Login to gitlab and store the token
func Login(token string, url string) error {
	git, err := gitlab.NewClient(token, gitlab.WithBaseURL(url))
	if err != nil {
		return err
	}

	_, r, err := git.Projects.ListProjects(&gitlab.ListProjectsOptions{
		Simple: gitlab.Ptr(true),
	})
	if err != nil {
		return err
	}

	slog.Info("successfull login", "url", url, "project_approx", r.ItemsPerPage*(r.TotalPages))
	err = storeToken(token, url)
	if err != nil {
		return err
	}
	return storeLastLoggedInDomain(url)
}

func GetClient(url string) (*gitlab.Client, error) {
	token, err := readToken(url)
	if err != nil {
		return nil, err
	}

	return gitlab.NewClient(token, gitlab.WithBaseURL(url))
}

func GetDefaultClient() (*gitlab.Client, error) {
	url, err := readLastLoggedInDomain()
	if err != nil {
		return nil, err
	}

	return GetClient(url)
}

func readToken(url string) (string, error) {
	if url == "" {
		url, err := readLastLoggedInDomain()
		if err != nil {
			return "", err
		}
		return readTokenForUrl(url)
	}
	return readTokenForUrl(url)
}

// storeToken stores the token in a file in the user's home directory per domain
func storeToken(token string, urlStr string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return err
	}

	tokenDir := filepath.Join(homeDir, ".gitlab-util", u.Hostname())
	err = os.MkdirAll(tokenDir, 0700)
	if err != nil {
		return err
	}

	tokenFile := filepath.Join(tokenDir, "token")
	err = os.WriteFile(tokenFile, []byte(token), 0600)
	if err != nil {
		return err
	}

	return nil
}

// readTokenForUrl reads the token from a file in the user's home directory per domain
func readTokenForUrl(urlStr string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return "", err
	}

	tokenFile := filepath.Join(homeDir, ".gitlab-util", u.Hostname(), "token")
	tokenBytes, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", err
	}

	return string(tokenBytes), nil
}

// storeLastLoggedInDomain stores the last logged-in domain in a file
func storeLastLoggedInDomain(urlStr string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	lastLoginFile := filepath.Join(homeDir, ".gitlab-util", "last_login")
	err = os.WriteFile(lastLoginFile, []byte(urlStr), 0600)
	if err != nil {
		return err
	}

	return nil
}

// readLastLoggedInDomain reads the last logged-in domain from a file
func readLastLoggedInDomain() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	lastLoginFile := filepath.Join(homeDir, ".gitlab-util", "last_login")
	urlBytes, err := os.ReadFile(lastLoginFile)
	if err != nil {
		return "", err
	}

	return string(urlBytes), nil
}
