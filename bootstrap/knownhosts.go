package bootstrap

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildkite/agent/bootstrap/shell"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh/knownhosts"
)

type knownHosts struct {
	Shell *shell.Shell
	Path  string
}

func findKnownHosts(sh *shell.Shell) (*knownHosts, error) {
	userHomePath, err := homedir.Dir()
	if err != nil {
		return nil, fmt.Errorf("Could not find the current users home directory (%s)", err)
	}

	// Construct paths to the known_hosts file
	sshDirectory := filepath.Join(userHomePath, ".ssh")
	knownHostPath := filepath.Join(sshDirectory, "known_hosts")

	// Ensure ssh directory exists
	if err := os.MkdirAll(sshDirectory, 0700); err != nil {
		return nil, err
	}

	// Ensure file exists
	if _, err := os.Stat(knownHostPath); err != nil {
		f, err := os.OpenFile(knownHostPath, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return nil, errors.Wrapf(err, "Could not create %q", knownHostPath)
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
	}

	return &knownHosts{Shell: sh, Path: knownHostPath}, nil
}

func (kh *knownHosts) Contains(host string) (bool, error) {
	file, err := os.Open(kh.Path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	normalized := knownhosts.Normalize(host)

	// There don't appear to be any libraries to parse known_hosts that don't also want to
	// validate the IP's and host keys. Shelling out to ssh-keygen doesn't support custom ports
	// so I guess we'll do it ourselves.
	//
	// known_host format is defined at https://man.openbsd.org/sshd#SSH_KNOWN_HOSTS_FILE_FORMAT
	// A basic example is:
	// # Comments allowed at start of line
	// closenet,...,192.0.2.53 1024 37 159...93 closenet.example.net
	// cvs.example.net,192.0.2.10 ssh-rsa AAAA1234.....=
	// # A hashed hostname
	// |1|JfKTdBh7rNbXkVAQCRp4OQoPfmI=|USECr3SWf1JUPsms5AqfD5QfxkM= ssh-rsa
	// AAAA1234.....=
	// # A revoked key
	// @revoked * ssh-rsa AAAAB5W...
	// # A CA key, accepted for any host in *.mydomain.com or *.mydomain.org
	// @cert-authority *.mydomain.org,*.mydomain.com ssh-rsa AAAAB5W...
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) != 3 {
			continue
		}
		for _, addr := range strings.Split(fields[0], ",") {
			if addr == normalized || addr == knownhosts.HashHostname(normalized) {
				return true, nil
			}
		}
	}

	return false, nil
}

func (kh *knownHosts) Add(host string) error {
	// Use a lockfile to prevent parallel processes stepping on each other
	lock, err := kh.Shell.LockFile(kh.Path+".lock", time.Second*30)
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			kh.Shell.Warningf("Failed to release known_hosts file lock: %#v", err)
		}
	}()

	// If the keygen output already contains the host, we can skip!
	if contains, _ := kh.Contains(host); contains {
		kh.Shell.Commentf("Host %q already in list of known hosts at \"%s\"", host, kh.Path)
		return nil
	}

	// Scan the key and then write it to the known_host file
	keyscanOutput, err := sshKeyScan(kh.Shell, host)
	if err != nil {
		return errors.Wrap(err, "Could not perform `ssh-keyscan`")
	}

	kh.Shell.Commentf("Added host %q to known hosts at \"%s\"", host, kh.Path)

	// Try and open the existing hostfile in (append_only) mode
	f, err := os.OpenFile(kh.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		return errors.Wrapf(err, "Could not open %q for appending", kh.Path)
	}
	defer f.Close()

	if _, err = fmt.Fprintf(f, "%s\n", keyscanOutput); err != nil {
		return errors.Wrapf(err, "Could not write to %q", kh.Path)
	}

	return nil
}

// AddFromRepository takes a git repo url, extracts the host and adds it
func (kh *knownHosts) AddFromRepository(repository string) error {
	u, err := parseGittableURL(repository)
	if err != nil {
		kh.Shell.Warningf("Could not parse %q as a URL - skipping adding host to SSH known_hosts", repository)
		return err
	}

	// We only need to keyscan ssh repository urls
	if u.Scheme != "ssh" {
		return nil
	}

	host := stripAliasesFromGitHost(u.Host)

	if err = kh.Add(host); err != nil {
		return errors.Wrapf(err, "Failed to add `%s` to known_hosts file `%s`", host, u)
	}

	return nil
}
