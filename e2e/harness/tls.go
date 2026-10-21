package harness

// The TLS e2e cases: a set of certificates is made during the test and a few server instances that
// take TLS alone are started with them -- a second PostgreSQL cluster, a second mysqld and two
// redis-servers. All of them live in temporary directories on ports FreePort handed out, and are
// stopped and cleaned up when the test ends: the instances running permanently on this host keep
// every setting they have, and the other e2e cases carry on.
//
// The certificates:
//
//	CA            the right CA, which signed the server certificate (127.0.0.1 / localhost / db.e2e.internal) and the client certificate (CN=xone_mtls)
//	OtherCA       an unrelated CA, which signed a "stranger" client certificate that is also CN=xone_mtls
//
// Without the matching server installed on this host (or with a redis-server built without TLS) it
// t.Skips and says what is missing.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// TLSPassword is the test account's password on the TLS instances. Asserting "no password in the
// error or in the log" compares against it
const TLSPassword = "tls-e2e-secret-pw"

// TLSServerName is the name on the server certificate besides 127.0.0.1 / localhost
const TLSServerName = "db.e2e.internal"

// TLSMTLSUser is the CN of the client certificate, and the database account that asks for a client
// certificate
const TLSMTLSUser = "xone_mtls"

// TLSCerts is one set of certificates just made, all of them files in Dir
type TLSCerts struct {
	Dir string

	CAFile      string // the right CA
	OtherCAFile string // the unrelated CA, against which the server certificate has to fail to verify

	ServerCert, ServerKey string // the server certificate and its private key

	ClientCert, ClientKey     string // the client certificate signed by the right CA (CN=xone_mtls)
	StrangerCert, StrangerKey string // the client certificate signed by the unrelated CA (CN=xone_mtls as well)

	// Pool holds the right CA alone, for a stub server to verify a client certificate with
	Pool *x509.CertPool
}

// NewTLSCerts makes a set of certificates. The directory and the files are readable by everyone:
// the postgres and mysql accounts have to read them (each private key is copied once more, with
// the owner and the permissions the server wants)
func NewTLSCerts(t testing.TB) *TLSCerts {
	t.Helper()
	dir := tempDir(t, "xone-tls-certs-")
	c := &TLSCerts{Dir: dir, Pool: x509.NewCertPool()}

	ca, caKey := newCA(t, "xone-e2e-ca")
	other, otherKey := newCA(t, "xone-e2e-other-ca")
	c.Pool.AddCert(ca)
	c.CAFile = writePEM(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	c.OtherCAFile = writePEM(t, dir, "other-ca.pem", "CERTIFICATE", other.Raw)

	srv, srvKey := issueCert(t, ca, caKey, 2, "localhost", x509.ExtKeyUsageServerAuth,
		[]string{"localhost", TLSServerName}, []net.IP{net.ParseIP("127.0.0.1")})
	c.ServerCert, c.ServerKey = writePair(t, dir, "server", srv, srvKey)
	cli, cliKey := issueCert(t, ca, caKey, 3, TLSMTLSUser, x509.ExtKeyUsageClientAuth, nil, nil)
	c.ClientCert, c.ClientKey = writePair(t, dir, "client", cli, cliKey)
	str, strKey := issueCert(t, other, otherKey, 4, TLSMTLSUser, x509.ExtKeyUsageClientAuth, nil, nil)
	c.StrangerCert, c.StrangerKey = writePair(t, dir, "stranger", str, strKey)
	return c
}

// ServerTLS is a *tls.Config using the server certificate, for an in-process stub server; with
// clientCA true it asks for a client certificate
func (c *TLSCerts) ServerTLS(t testing.TB, clientCA bool) *tls.Config {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(c.ServerCert, c.ServerKey)
	if err != nil {
		t.Fatalf("load server certificate: %v", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if clientCA {
		cfg.ClientCAs, cfg.ClientAuth = c.Pool, tls.RequireAndVerifyClientCert
	}
	return cfg
}

func newCA(t testing.TB, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	ca, _ := x509.ParseCertificate(der)
	return ca, key
}

func issueCert(t testing.TB, ca *x509.Certificate, caKey *ecdsa.PrivateKey, serial int64, cn string,
	usage x509.ExtKeyUsage, dns []string, ips []net.IP) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: dns, IPAddresses: ips,
		ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("issue certificate: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key
}

func writePair(t testing.TB, dir, name string, cert *x509.Certificate, key *ecdsa.PrivateKey) (string, string) {
	t.Helper()
	// PKCS#8: PG, MySQL (OpenSSL), Redis and Go all take it
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return writePEM(t, dir, name+".pem", "CERTIFICATE", cert.Raw), writePEM(t, dir, name+"-key.pem", "PRIVATE KEY", der)
}

func writePEM(t testing.TB, dir, name, typ string, der []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// tempDir is a temporary directory everyone can enter (t.TempDir is 0700, which the postgres /
// mysql accounts cannot enter), deleted when the test ends
func tempDir(t testing.TB, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// chownTo hands path over to the system account name
func chownTo(t testing.TB, name string, paths ...string) {
	t.Helper()
	u, err := user.Lookup(name)
	if err != nil {
		t.Skipf("TLS e2e needs the %q system account: %v", name, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	for _, p := range paths {
		if err := os.Chown(p, uid, gid); err != nil {
			t.Fatalf("chown %s to %s: %v", p, name, err)
		}
	}
}

// copyFile makes a copy with the permissions set to mode
func copyFile(t testing.TB, from, to string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	if err := os.WriteFile(to, b, mode); err != nil {
		t.Fatalf("write %s: %v", to, err)
	}
}

// run runs one command and takes its output into t.Fatal on failure (the output holds paths and
// ports alone and no password: every password is handed over through a file or through SQL on a
// socket)
func run(t testing.TB, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// waitTCP waits until addr connects, for timeout at most
func waitTCP(t testing.TB, addr string, timeout time.Duration, logFile string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(logFile)
			t.Fatalf("%s did not come up within %v: %v\n%s", addr, timeout, err, log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---- PostgreSQL ----

// TLSPostgres is a PostgreSQL cluster that takes TLS alone
//
// pg_hba holds hostssl alone: a plaintext connection is told no pg_hba.conf entry ... no
// encryption. The account postgres logs in with a password; the account xone_mtls has to present a
// client certificate whose CN is the account name as well (clientcert=verify-full). Both passwords
// are TLSPassword and the database is postgres
type TLSPostgres struct {
	Addr string
}

// DSN is the key=value DSN connecting to this cluster as the account user, carrying no ssl
// parameter at all (TLS is left to the TLS block in the configuration)
func (p *TLSPostgres) DSN(user string) string {
	host, port, _ := net.SplitHostPort(p.Addr)
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=postgres", host, port, user, TLSPassword)
}

// StartTLSPostgres initdbs a new cluster and starts it with ssl=on on a random port, then stops
// and deletes it when the test ends
func StartTLSPostgres(t testing.TB, certs *TLSCerts) *TLSPostgres {
	t.Helper()
	Require(t)
	bin := pgBinDir(t)
	dir := tempDir(t, "xone-tls-pg-")
	data := filepath.Join(dir, "data")
	pwfile := filepath.Join(dir, "pw")
	copyFile(t, certs.ServerCert, filepath.Join(dir, "server.crt"), 0o644)
	copyFile(t, certs.ServerKey, filepath.Join(dir, "server.key"), 0o600) // PG wants the private key readable by its owner alone
	copyFile(t, certs.CAFile, filepath.Join(dir, "ca.crt"), 0o644)
	if err := os.WriteFile(pwfile, []byte(TLSPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chownTo(t, "postgres", dir, pwfile, filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), filepath.Join(dir, "ca.crt"))

	asPG := func(args ...string) { run(t, "runuser", append([]string{"-u", "postgres", "--"}, args...)...) }
	asPG(filepath.Join(bin, "initdb"), "-D", data, "-U", "postgres", "--pwfile="+pwfile,
		"--auth=scram-sha-256", "-E", "UTF8", "--no-sync")

	port := FreePort(t)
	conf := fmt.Sprintf(`
listen_addresses = '127.0.0.1'
port = %d
unix_socket_directories = '%s'
ssl = on
ssl_cert_file = '%s/server.crt'
ssl_key_file = '%s/server.key'
ssl_ca_file = '%s/ca.crt'
fsync = off
`, port, dir, dir, dir, dir)
	appendFile(t, filepath.Join(data, "postgresql.conf"), conf)
	hba := "local all postgres scram-sha-256\n" +
		"hostssl all " + TLSMTLSUser + " 127.0.0.1/32 scram-sha-256 clientcert=verify-full\n" +
		"hostssl all all 127.0.0.1/32 scram-sha-256\n"
	if err := os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte(hba), 0o600); err != nil {
		t.Fatal(err)
	}
	chownTo(t, "postgres", filepath.Join(data, "pg_hba.conf"))

	logFile := filepath.Join(dir, "postgres.log")
	asPG(filepath.Join(bin, "pg_ctl"), "-D", data, "-l", logFile, "-w", "-t", "30", "start")
	t.Cleanup(func() {
		out, err := exec.Command("runuser", "-u", "postgres", "--", filepath.Join(bin, "pg_ctl"), "-D", data, "-m", "immediate", "-w", "stop").CombinedOutput()
		if err != nil {
			t.Logf("stop the TLS PostgreSQL cluster: %v\n%s", err, out)
		}
	})

	p := &TLSPostgres{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	// Create xone_mtls: over the Unix socket as postgres, with the password in PGPASSWORD rather than on the command line
	cmd := exec.Command("runuser", "-u", "postgres", "--", filepath.Join(bin, "psql"), "-h", dir, "-p", strconv.Itoa(port),
		"-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-q", "-c",
		"CREATE ROLE "+TLSMTLSUser+" LOGIN PASSWORD '"+TLSPassword+"'")
	cmd.Env = append(os.Environ(), "PGPASSWORD="+TLSPassword)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create role %s: %v\n%s", TLSMTLSUser, err, out)
	}
	return p
}

// pgBinDir is this host's PostgreSQL bin directory: XONE_E2E_PG_BINDIR, or the highest version
// under /usr/lib/postgresql/*/bin
func pgBinDir(t testing.TB) string {
	t.Helper()
	if d := os.Getenv("XONE_E2E_PG_BINDIR"); d != "" {
		return d
	}
	dirs, _ := filepath.Glob("/usr/lib/postgresql/*/bin")
	best, bestVer := "", -1
	for _, d := range dirs {
		v, err := strconv.Atoi(filepath.Base(filepath.Dir(d)))
		if err == nil && v > bestVer && fileExists(filepath.Join(d, "initdb")) {
			best, bestVer = d, v
		}
	}
	if best == "" {
		t.Skip("TLS e2e needs PostgreSQL's initdb / pg_ctl (not found under /usr/lib/postgresql/*/bin; set XONE_E2E_PG_BINDIR)")
	}
	return best
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func appendFile(t testing.TB, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// ---- MySQL ----

// TLSMySQL is a mysqld that takes TLS alone (require_secure_transport=ON, and a plaintext
// connection is told 3159)
//
// The accounts are xone (mysql_native_password, refusing plaintext through
// require_secure_transport) and xone_mtls (REQUIRE X509, which has to present a client certificate
// signed by the right CA), both with the password TLSPassword, and the database is xone_tls
type TLSMySQL struct {
	Addr string
}

// DSN connects to this instance as the account user, carrying no tls parameter (TLS is left to the
// TLS block in the configuration)
func (m *TLSMySQL) DSN(user string) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/xone_tls", user, TLSPassword, m.Addr)
}

// StartTLSMySQL --initialize-insecures a new instance in a temporary directory and starts it on a
// random port with our certificates
func StartTLSMySQL(t testing.TB, certs *TLSCerts) *TLSMySQL {
	t.Helper()
	Require(t)
	mysqld, err := exec.LookPath("mysqld")
	if err != nil {
		if mysqld, err = exec.LookPath("/usr/sbin/mysqld"); err != nil {
			t.Skip("TLS e2e needs mysqld (not found in PATH or /usr/sbin)")
		}
	}
	dir := tempDir(t, "xone-tls-mysql-")
	for _, f := range []struct{ from, to string }{
		{certs.ServerCert, "server.pem"}, {certs.ServerKey, "server-key.pem"}, {certs.CAFile, "ca.pem"},
	} {
		copyFile(t, f.from, filepath.Join(dir, f.to), 0o600)
		chownTo(t, "mysql", filepath.Join(dir, f.to))
	}
	chownTo(t, "mysql", dir)
	data := filepath.Join(dir, "data")
	run(t, mysqld, "--no-defaults", "--initialize-insecure", "--user=mysql", "--datadir="+data,
		"--log-error="+filepath.Join(dir, "init.log"))

	port := FreePort(t)
	sock := filepath.Join(dir, "mysqld.sock")
	logFile := filepath.Join(dir, "mysqld.log")
	cmd := exec.Command(mysqld, "--no-defaults", "--user=mysql", "--datadir="+data,
		"--port="+strconv.Itoa(port), "--bind-address=127.0.0.1", "--socket="+sock, "--mysqlx=OFF",
		"--pid-file="+filepath.Join(dir, "mysqld.pid"), "--log-error="+logFile, "--skip-log-bin",
		"--innodb-buffer-pool-size=16M", "--performance-schema=OFF",
		"--ssl-ca="+filepath.Join(dir, "ca.pem"), "--ssl-cert="+filepath.Join(dir, "server.pem"),
		"--ssl-key="+filepath.Join(dir, "server-key.pem"), "--require-secure-transport=ON")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mysqld: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	waitTCP(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 60*time.Second, logFile)

	// Create the accounts and the database over the Unix socket as root (--initialize-insecure: no
	// password). require_secure_transport does not stop a socket: it counts as a secure transport
	root, err := sql.Open("mysql", (&mysql.Config{User: "root", Net: "unix", Addr: sock, AllowNativePasswords: true}).FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, stmt := range []string{
		"CREATE DATABASE xone_tls",
		// For require_secure_transport to be the one speaking on a plaintext connection (3159), the
		// account must carry no other obstacle: measured on 8.0.46, an account with REQUIRE SSL over
		// plaintext and the default caching_sha2_password over plaintext both answer 1045 Access
		// denied first, which does not tell that an insecure transport was what got refused. So this
		// account carries no REQUIRE and uses mysql_native_password
		"CREATE USER 'xone'@'127.0.0.1' IDENTIFIED WITH mysql_native_password BY '" + TLSPassword + "'",
		"CREATE USER '" + TLSMTLSUser + "'@'127.0.0.1' IDENTIFIED BY '" + TLSPassword + "' REQUIRE X509",
		"GRANT ALL ON xone_tls.* TO 'xone'@'127.0.0.1'",
		"GRANT ALL ON xone_tls.* TO '" + TLSMTLSUser + "'@'127.0.0.1'",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := root.ExecContext(ctx, stmt)
		cancel()
		if err != nil {
			t.Fatalf("set up the TLS mysqld (%s…): %v", strings.Fields(stmt)[0], err)
		}
	}
	return &TLSMySQL{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
}

// ---- Redis ----

// TLSRedis is two redis-servers with their TLS port alone open (port 0): Addr asks for no client
// certificate and MTLSAddr does (tls-auth-clients yes). Both passwords are TLSPassword
type TLSRedis struct {
	Addr, MTLSAddr string
}

// StartTLSRedis starts two redis-servers and stops them when the test ends. With a redis-server
// built without TLS it t.Skips
func StartTLSRedis(t testing.TB, certs *TLSCerts) *TLSRedis {
	t.Helper()
	Require(t)
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("TLS e2e needs redis-server (not found in PATH)")
	}
	dir := tempDir(t, "xone-tls-redis-")
	start := func(authClients string) string {
		port := FreePort(t)
		logFile := filepath.Join(dir, "redis-"+strconv.Itoa(port)+".log")
		cmd := exec.Command(bin, "--port", "0", "--tls-port", strconv.Itoa(port), "--bind", "127.0.0.1",
			"--tls-cert-file", certs.ServerCert, "--tls-key-file", certs.ServerKey, "--tls-ca-cert-file", certs.CAFile,
			"--tls-auth-clients", authClients, "--save", "", "--appendonly", "no", "--dir", dir, "--logfile", logFile)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		// The password is handed over through the configuration on stdin rather than on the command line (which ps shows)
		cmd.Args = append(cmd.Args[:1], append([]string{"-"}, cmd.Args[1:]...)...)
		cmd.Stdin = strings.NewReader("requirepass " + TLSPassword + "\n")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start redis-server: %v", err)
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			<-exited
		})
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		deadline := time.Now().Add(10 * time.Second)
		for {
			select {
			case <-exited:
				log, _ := os.ReadFile(logFile)
				msg := out.String() + string(log)
				if strings.Contains(msg, "tls-port") || strings.Contains(strings.ToLower(msg), "tls") {
					t.Skipf("redis-server was built without TLS support, skipping: %s", strings.TrimSpace(msg))
				}
				t.Fatalf("redis-server exited: %s", msg)
			default:
			}
			if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
				c.Close()
				return addr
			}
			if time.Now().After(deadline) {
				t.Fatalf("redis-server on %s did not come up", addr)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return &TLSRedis{Addr: start("no"), MTLSAddr: start("yes")}
}

// SelfSignedCert generates a self-signed certificate for 127.0.0.1 / localhost in dir, and returns
// the paths of the certificate and the private key, and a CertPool trusting that certificate alone
func SelfSignedCert(t testing.TB, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "xone e2e"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}
