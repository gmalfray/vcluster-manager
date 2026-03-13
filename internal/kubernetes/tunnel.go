package kubernetes

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// SSHTunnel represents an SSH port-forwarding tunnel to a remote Kubernetes API server.
type SSHTunnel struct {
	listener   net.Listener
	sshClient  *ssh.Client
	localAddr  string
	remoteAddr string
	done       chan struct{}
	mu         sync.Mutex
}

// NewSSHTunnel creates a local TCP listener that forwards connections through an SSH tunnel
// to the remote Kubernetes API server.
//
// sshTarget format: "user@host:port" (e.g. "deploy@bastion.example.com:22226")
// sshKeyPath: path to the SSH private key
// remoteK8sAddr: the K8s API server address as seen from the SSH host (e.g. "10.0.0.1:6443")
func NewSSHTunnel(sshTarget, sshKeyPath, remoteK8sAddr string) (*SSHTunnel, error) {
	user, host, err := parseSSHTarget(sshTarget)
	if err != nil {
		return nil, fmt.Errorf("parsing SSH target %q: %w", sshTarget, err)
	}

	keyData, err := os.ReadFile(sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading SSH key %s: %w", sshKeyPath, err)
	}

	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("parsing SSH key: %w", err)
	}

	sshConfig := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // bastion in trusted network
	}

	sshClient, err := ssh.Dial("tcp", host, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("SSH dial to %s: %w", host, err)
	}

	// Listen on a random local port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("creating local listener: %w", err)
	}

	t := &SSHTunnel{
		listener:   listener,
		sshClient:  sshClient,
		localAddr:  listener.Addr().String(),
		remoteAddr: remoteK8sAddr,
		done:       make(chan struct{}),
	}

	go t.acceptLoop()

	log.Printf("SSH tunnel established: %s → %s → %s", t.localAddr, host, remoteK8sAddr)
	return t, nil
}

// LocalAddr returns the local address of the tunnel (e.g. "127.0.0.1:54321").
func (t *SSHTunnel) LocalAddr() string {
	return t.localAddr
}

// Close shuts down the SSH tunnel.
func (t *SSHTunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	select {
	case <-t.done:
		return nil // already closed
	default:
		close(t.done)
	}

	var errs []error
	if err := t.listener.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := t.sshClient.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("closing SSH tunnel: %v", errs)
	}
	return nil
}

func (t *SSHTunnel) acceptLoop() {
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.done:
				return
			default:
				log.Printf("SSH tunnel accept error: %v", err)
				return
			}
		}
		go t.handleConn(conn)
	}
}

func (t *SSHTunnel) handleConn(localConn net.Conn) {
	remoteConn, err := t.sshClient.Dial("tcp", t.remoteAddr)
	if err != nil {
		log.Printf("SSH tunnel dial to %s: %v", t.remoteAddr, err)
		localConn.Close()
		return
	}

	go func() {
		defer localConn.Close()
		defer remoteConn.Close()
		io.Copy(localConn, remoteConn) //nolint:errcheck
	}()
	go func() {
		defer localConn.Close()
		defer remoteConn.Close()
		io.Copy(remoteConn, localConn) //nolint:errcheck
	}()
}

// parseSSHTarget parses "user@host:port" into user and host:port.
func parseSSHTarget(target string) (string, string, error) {
	parts := strings.SplitN(target, "@", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("expected format user@host:port, got %q", target)
	}
	user := parts[0]
	host := parts[1]

	// Ensure host has a port
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = host + ":22"
	}

	return user, host, nil
}
