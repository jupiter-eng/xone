package harness

// The ClickHouse for the TLS e2e cases: a container of its own, used by this group alone (xone-ch
// keeps every setting it has), with tcp_port_secure and https_port on, the certificates the ones
// NewTLSCerts just made, and the container removed when the test ends.
//
// The plaintext ports (tcp_port / http_port) are open in the same container: both "a TLS block is
// on but it connects to the plaintext port" and "no TLS block but it connects to the TLS port"
// have to be measured against a real server.
//
// Without docker, or with the image xone-ch uses missing on this host, it t.Skips and says what is
// missing -- images are not pulled from a test: one pull takes minutes, and CI has no ClickHouse.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TLSCHImage is the image of the ClickHouse container for TLS, the same one as xone-ch
func TLSCHImage() string {
	return env("XONE_E2E_CH_IMAGE", "mirror.gcr.io/clickhouse/clickhouse-server:24.8")
}

// TLSCHUser / TLSCHDB are the account logging in with a password on the TLS ClickHouse and its
// database. The password is TLSPassword
const (
	TLSCHUser = "xone"
	TLSCHDB   = "xone_tls"
)

// TLSClickHouse is a ClickHouse container with its TLS ports open, each published to 127.0.0.1 on
// this host under the same number it listens on inside
//
// The account xone logs in with the password TLSPassword; the account xone_mtls has no password and
// logs in by presenting a client certificate with CN=xone_mtls signed by the right CA
// (ssl_certificates). The server's verificationMode is relaxed: it asks for a client certificate
// without insisting, and checks one that is presented against the CA.
type TLSClickHouse struct {
	Name string // the container name

	NativeTLS string // tcp_port_secure
	HTTPS     string // https_port
	Native    string // tcp_port (plaintext)
	HTTP      string // http_port (plaintext)
}

// DSN connects to addr with scheme (clickhouse / https / http) as the account xone, carrying no TLS
// parameter at all (TLS is left to the TLS block in the configuration)
func (c *TLSClickHouse) DSN(scheme, addr string) string {
	return fmt.Sprintf("%s://%s:%s@%s/%s", scheme, TLSCHUser, TLSPassword, addr, TLSCHDB)
}

// MTLSDSN connects to addr as the account xone_mtls (certificate authentication, no password)
func (c *TLSClickHouse) MTLSDSN(scheme, addr string) string {
	return fmt.Sprintf("%s://%s@%s/%s", scheme, TLSMTLSUser, addr, TLSCHDB)
}

// StartTLSClickHouse starts the container and waits until all four ports connect, then docker rm -f
// it when the test ends
func StartTLSClickHouse(t testing.TB, certs *TLSCerts) *TLSClickHouse {
	t.Helper()
	Require(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("ClickHouse TLS e2e needs docker (not found in PATH)")
	}
	if out, err := docker(30*time.Second, "image", "inspect", "--format", "{{.Id}}", TLSCHImage()); err != nil {
		t.Skipf("ClickHouse TLS e2e needs the image %s locally (docker pull it first): %v %s", TLSCHImage(), err, out)
	}

	dir := tempDir(t, "xone-tls-ch-")
	for _, f := range []struct{ from, to string }{
		{certs.ServerCert, "server.pem"}, {certs.ServerKey, "server-key.pem"}, {certs.CAFile, "ca.pem"},
	} {
		// The clickhouse account in the container (uid 101) has to be able to read them; the directory exists for the test alone
		copyFile(t, f.from, filepath.Join(dir, f.to), 0o644)
	}

	c := &TLSClickHouse{Name: "xone-ch-tls-" + NewID()}
	nativeTLS, https, native, http := FreePort(t), FreePort(t), FreePort(t), FreePort(t)
	addr := func(p int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(p)) }
	c.NativeTLS, c.HTTPS, c.Native, c.HTTP = addr(nativeTLS), addr(https), addr(native), addr(http)

	// Override the docker_related_config.xml the image carries (which listens on 0.0.0.0 and on ::):
	// IPv4 alone, since the ports below are published over IPv4 and an unreachable :: only fills the
	// server log. The container has a network namespace of its own, so 0.0.0.0 here is reachable
	// through the published ports and nowhere else.
	listen := `<clickhouse><listen_host>0.0.0.0</listen_host></clickhouse>`
	conf := fmt.Sprintf(`<clickhouse>
  <tcp_port>%d</tcp_port>
  <http_port>%d</http_port>
  <tcp_port_secure>%d</tcp_port_secure>
  <https_port>%d</https_port>
  <mysql_port remove="1"/>
  <postgresql_port remove="1"/>
  <interserver_http_port remove="1"/>
  <openSSL>
    <server>
      <certificateFile>/xone-tls/server.pem</certificateFile>
      <privateKeyFile>/xone-tls/server-key.pem</privateKeyFile>
      <caConfig>/xone-tls/ca.pem</caConfig>
      <loadDefaultCAFile>false</loadDefaultCAFile>
      <verificationMode>relaxed</verificationMode>
    </server>
  </openSSL>
</clickhouse>`, native, http, nativeTLS, https)
	users := fmt.Sprintf(`<clickhouse><users><%[1]s>
  <ssl_certificates><common_name>%[1]s</common_name></ssl_certificates>
  <networks><ip>::/0</ip></networks>
  <profile>default</profile><quota>default</quota>
</%[1]s></users></clickhouse>`, TLSMTLSUser)
	for name, text := range map[string]string{"listen.xml": listen, "xone-tls.xml": conf, "mtls-user.xml": users} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The password is handed over through --env-file and never on the command line (which ps shows)
	envFile := filepath.Join(dir, "env")
	envText := "CLICKHOUSE_USER=" + TLSCHUser + "\nCLICKHOUSE_PASSWORD=" + TLSPassword + "\n"
	if err := os.WriteFile(envFile, []byte(envText), 0o600); err != nil {
		t.Fatal(err)
	}
	// The ports are published rather than shared through --network host. Host networking is a Linux
	// feature: on Docker Desktop (macOS, Windows) the containers run inside a Linux VM, so "host" is
	// that VM's network namespace and nothing the container listens on is reachable at 127.0.0.1
	// here -- measured, the container comes up and every dial from the test is refused, which this
	// case used to sit out for the whole 60s. Publishing works the same everywhere.
	// Each port is published to the same number it listens on, so the numbers FreePort handed out
	// stay the ones in c.NativeTLS and friends, and to 127.0.0.1 alone so nothing is exposed.
	args := []string{"run", "-d", "--name", c.Name, "--env-file", envFile,
		"--tmpfs", "/var/lib/clickhouse",
		"-v", dir + "/listen.xml:/etc/clickhouse-server/config.d/docker_related_config.xml:ro",
		"-v", dir + "/xone-tls.xml:/etc/clickhouse-server/config.d/xone-tls.xml:ro",
		"-v", dir + "/mtls-user.xml:/etc/clickhouse-server/users.d/xone-mtls.xml:ro",
		"-v", dir + ":/xone-tls:ro"}
	for _, p := range []int{nativeTLS, https, native, http} {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", p, p))
	}
	args = append(args, TLSCHImage())
	if out, err := docker(2*time.Minute, args...); err != nil {
		t.Fatalf("docker run the TLS ClickHouse: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := docker(time.Minute, "rm", "-f", "-v", c.Name); err != nil {
			t.Logf("remove the TLS ClickHouse container %s: %v\n%s", c.Name, err, out)
		}
	})

	deadline := time.Now().Add(60 * time.Second)
	for _, a := range []string{c.NativeTLS, c.HTTPS, c.Native, c.HTTP} {
		for {
			conn, err := net.DialTimeout("tcp", a, 200*time.Millisecond)
			if err == nil {
				conn.Close()
				break
			}
			if time.Now().After(deadline) {
				logs, _ := docker(30*time.Second, "logs", "--tail", "50", c.Name)
				t.Fatalf("TLS ClickHouse %s did not come up within 60s: %v\n%s", a, err, logs)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// The database is created here rather than through CLICKHOUSE_DB: the clickhouse-client the entry
	// point script calls to create it carries no --port and so goes to 9000, while every port of this
	// server is one FreePort handed out. This goes through the container's own clickhouse-client to
	// this container's plaintext port, with the password in the environment rather than on the
	// command line
	for {
		out, err := dockerEnv(30*time.Second, []string{"CLICKHOUSE_PASSWORD=" + TLSPassword},
			"exec", "-e", "CLICKHOUSE_PASSWORD", c.Name, "clickhouse-client", "--port", strconv.Itoa(native),
			"--user", TLSCHUser, "-q", "CREATE DATABASE IF NOT EXISTS "+TLSCHDB)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			logs, _ := docker(30*time.Second, "logs", "--tail", "50", c.Name)
			t.Fatalf("TLS ClickHouse: create database %s: %v %s\n%s", TLSCHDB, err, out, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Log is the lines of the container's server log holding needle (limit lines at most)
func (c *TLSClickHouse) Log(needle string, limit int) []string {
	out, _ := docker(30*time.Second, "exec", c.Name, "grep", "-F", needle, "/var/log/clickhouse-server/clickhouse-server.log")
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" && len(lines) < limit {
			lines = append(lines, l)
		}
	}
	return lines
}

func docker(timeout time.Duration, args ...string) (string, error) {
	return dockerEnv(timeout, nil, args...)
}

func dockerEnv(timeout time.Duration, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
