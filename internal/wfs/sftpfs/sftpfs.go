package sftpfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pkg/sftp"
	"gitlab.com/rhogenson/ccp/internal/wfs"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

var (
	_ wfs.FS         = (*FS)(nil)
	_ wfs.ReadLinkFS = (*FS)(nil)
	_ fs.StatFS      = (*FS)(nil)
	_ fs.StatFS      = (*FS)(nil)
	_ fs.ReadDirFS   = (*FS)(nil)
)

type FS struct {
	User, Host string
	conn       *sftp.Client
	sshConn    *ssh.Client
}

var sshAgent = sync.OnceValue(func() agent.ExtendedAgent {
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		return nil
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil
	}
	return agent.NewClient(conn)
})

func sshKeys() ([]ssh.Signer, error) {
	sshAgent := sshAgent()
	if sshAgent != nil {
		if signers, err := sshAgent.Signers(); err == nil && len(signers) > 0 {
			return signers, nil
		}
	}
	sshDir := filepath.Join(os.Getenv("HOME"), ".ssh")
	sshFiles, err := os.ReadDir(sshDir)
	if len(sshFiles) == 0 {
		return nil, err
	}
	var keys []ssh.Signer
	var passwordProtectedKey []byte
	var passwordProtectedKeyFile string
	for _, f := range sshFiles {
		if f.Name() == "known_hosts" || strings.HasSuffix(f.Name(), ".pub") {
			continue
		}
		fileName := filepath.Join(sshDir, f.Name())
		keyBytes, err := os.ReadFile(fileName)
		if err != nil {
			continue
		}
		key, err := ssh.ParsePrivateKey(keyBytes)
		if err != nil {
			if passwordProtectedKey == nil && errors.As(err, new(*ssh.PassphraseMissingError)) {
				passwordProtectedKey = keyBytes
				passwordProtectedKeyFile = fileName
			}
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 && passwordProtectedKey != nil {
		fmt.Fprintf(os.Stderr, "Enter password for %s: ", passwordProtectedKeyFile)
		for i := range 3 {
			if i > 0 {
				fmt.Fprintf(os.Stderr, "Incorrect password, try again: ")
			}
			password, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return nil, err
			}
			key, err := ssh.ParseRawPrivateKeyWithPassphrase(passwordProtectedKey, password)
			if err != nil {
				continue
			}
			if sshAgent != nil {
				sshAgent.Add(agent.AddedKey{PrivateKey: key})
			}
			signer, err := ssh.NewSignerFromKey(key)
			if err != nil {
				return nil, err
			}
			return []ssh.Signer{signer}, nil
		}
		return nil, errors.New("user couldn't remember her password")
	}
	return keys, nil
}

func appendToKnownHosts(hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(path.Join(os.Getenv("HOME"), ".ssh/known_hosts"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(knownhosts.Line([]string{hostname}, key) + "\n"); err != nil {
		return err
	}
	return f.Close()
}

func Dial(target string) (*FS, error) {
	knownHostChecker, err := knownhosts.New(path.Join(os.Getenv("HOME"), ".ssh/known_hosts"))
	if err != nil {
		knownHostChecker = func(string, net.Addr, ssh.PublicKey) error { return &knownhosts.KeyError{} }
	}
	var user string
	if i := strings.Index(target, "@"); i >= 0 {
		user, target = target[:i], target[i+1:]
	} else {
		user = os.Getenv("USER")
	}
	sshConn, err := ssh.Dial("tcp", target+":22", &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeysCallback(sshKeys),
			ssh.RetryableAuthMethod(ssh.PasswordCallback(func() (string, error) {
				fmt.Fprintf(os.Stderr, "Enter password for %s@%s: ", user, target)
				password, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(os.Stderr)
				return string(password), err
			}), 3),
		},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			err := knownHostChecker(hostname, remote, key)
			if err == nil {
				return nil
			}
			var keyErr *knownhosts.KeyError
			if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
				return err
			}
			appendToKnownHosts(hostname, key)
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	sftpConn, err := sftp.NewClient(sshConn)
	if err != nil {
		sshConn.Close()
		return nil, err
	}
	return &FS{
		User:    user,
		Host:    target,
		conn:    sftpConn,
		sshConn: sshConn,
	}, nil
}

func (f *FS) Close() error {
	sftpErr := f.conn.Close()
	if err := f.sshConn.Close(); err != nil {
		return err
	}
	return sftpErr
}

func (f *FS) Open(name string) (fs.File, error) {
	return f.conn.Open(name)
}

func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	entriesFileInfo, err := f.conn.ReadDir(name)
	entries := make([]fs.DirEntry, len(entriesFileInfo))
	for i, entry := range entriesFileInfo {
		entries[i] = fs.FileInfoToDirEntry(entry)
	}
	return entries, err
}

func (f *FS) Stat(name string) (fs.FileInfo, error) {
	return f.conn.Stat(name)
}

func (f *FS) Lstat(name string) (fs.FileInfo, error) {
	return f.conn.Lstat(name)
}

func (f *FS) ReadLink(name string) (string, error) {
	return f.conn.ReadLink(name)
}

func (f *FS) Create(name string, perm fs.FileMode) (io.WriteCloser, error) {
	file, err := f.conn.Create(name)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(perm); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (f *FS) Remove(name string) error {
	return f.conn.Remove(name)
}

func (f *FS) Mkdir(name string) error {
	return f.conn.Mkdir(name)
}

func (f *FS) Symlink(oldname, newname string) error {
	return f.conn.Symlink(oldname, newname)
}

func (f *FS) Chmod(name string, mode fs.FileMode) error {
	return f.conn.Chmod(name, mode)
}
