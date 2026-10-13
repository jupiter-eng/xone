package harness

// TLS 的 e2e：测试时现造一套证书，再起几个只收 TLS 的服务端实例——
// 第二个 PostgreSQL 集群、第二个 mysqld、两个 redis-server。都在临时目录里，
// 端口是 FreePort 给的，测试结束时停掉、删干净：本机常驻的那几个实例一个配置都不动，
// 别的 e2e 照常跑。
//
// 证书：
//
//	CA            正确的 CA，签了服务端证书（127.0.0.1 / localhost / db.e2e.internal）和客户端证书（CN=xone_mtls）
//	OtherCA       一个不相干的 CA，签了一张 CN=xone_mtls 的「陌生人」客户端证书
//
// 本机没装对应的服务端（或 redis-server 没带 TLS）时 t.Skip，并说清缺的是什么。

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

// TLSPassword 几个 TLS 实例上测试账号的密码。断言「错误和日志里没有密码」时拿它比对
const TLSPassword = "tls-e2e-secret-pw"

// TLSServerName 服务端证书上除 127.0.0.1 / localhost 之外的那个名字
const TLSServerName = "db.e2e.internal"

// TLSMTLSUser 客户端证书的 CN，也是要求客户端证书的那个数据库账号
const TLSMTLSUser = "xone_mtls"

// TLSCerts 一套现造的证书，文件都在 Dir 里
type TLSCerts struct {
	Dir string

	CAFile      string // 正确的 CA
	OtherCAFile string // 不相干的 CA，拿它校验服务端证书必须失败

	ServerCert, ServerKey string // 服务端证书与私钥

	ClientCert, ClientKey     string // 正确的 CA 签的客户端证书（CN=xone_mtls）
	StrangerCert, StrangerKey string // 不相干的 CA 签的客户端证书（同样 CN=xone_mtls）

	// Pool 只含正确的 CA，桩服务端校验客户端证书用
	Pool *x509.CertPool
}

// NewTLSCerts 现造一套证书。目录和文件对所有人可读：postgres、mysql 两个账号要读它们
// （私钥各自再复制一份、按服务端的要求改属主和权限）
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

// ServerTLS 用服务端证书的 *tls.Config，给进程内的桩服务端用；clientCA 为 true 时要求客户端证书
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
	// PKCS#8：PG、MySQL（OpenSSL）、Redis、Go 都认
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

// tempDir 一个所有人都进得去的临时目录（t.TempDir 是 0700，postgres / mysql 账号进不去），测试结束删掉
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

// chownTo 把 path 交给系统账号 name
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

// copyFile 复制一份，权限设成 mode
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

// run 跑一条命令，失败时把输出带进 t.Fatal（输出里只有路径和端口，没有密码：
// 密码都经文件或 socket 上的 SQL 交过去）
func run(t testing.TB, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// waitTCP 等 addr 能连上，最多 timeout
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

// TLSPostgres 一个只收 TLS 的 PostgreSQL 集群
//
// pg_hba 里只有 hostssl：明文连过来是 no pg_hba.conf entry … no encryption。
// 账号 postgres 用密码登录；账号 xone_mtls 另外要求出示 CN 与账号同名的客户端证书
// （clientcert=verify-full）。密码都是 TLSPassword，库是 postgres
type TLSPostgres struct {
	Addr string
}

// DSN 用账号 user 连这个集群的 key=value DSN，不带任何 ssl 参数（TLS 交给配置里的 TLS 块）
func (p *TLSPostgres) DSN(user string) string {
	host, port, _ := net.SplitHostPort(p.Addr)
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=postgres", host, port, user, TLSPassword)
}

// StartTLSPostgres initdb 一个新集群、开 ssl=on 起在随机端口上，测试结束时停掉删掉
func StartTLSPostgres(t testing.TB, certs *TLSCerts) *TLSPostgres {
	t.Helper()
	Require(t)
	bin := pgBinDir(t)
	dir := tempDir(t, "xone-tls-pg-")
	data := filepath.Join(dir, "data")
	pwfile := filepath.Join(dir, "pw")
	copyFile(t, certs.ServerCert, filepath.Join(dir, "server.crt"), 0o644)
	copyFile(t, certs.ServerKey, filepath.Join(dir, "server.key"), 0o600) // PG 要求私钥只有属主能读
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
	// 建 xone_mtls：经 Unix socket 以 postgres 登录，密码走 PGPASSWORD，不上命令行
	cmd := exec.Command("runuser", "-u", "postgres", "--", filepath.Join(bin, "psql"), "-h", dir, "-p", strconv.Itoa(port),
		"-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-q", "-c",
		"CREATE ROLE "+TLSMTLSUser+" LOGIN PASSWORD '"+TLSPassword+"'")
	cmd.Env = append(os.Environ(), "PGPASSWORD="+TLSPassword)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create role %s: %v\n%s", TLSMTLSUser, err, out)
	}
	return p
}

// pgBinDir 本机 PostgreSQL 的 bin 目录：XONE_E2E_PG_BINDIR，或者 /usr/lib/postgresql/*/bin 里版本最高的那个
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

// TLSMySQL 一个只收 TLS 的 mysqld（require_secure_transport=ON，明文连过来报 3159）
//
// 账号 xone（mysql_native_password，靠 require_secure_transport 拒明文）和 xone_mtls（REQUIRE X509，要出示正确的 CA 签的客户端证书），
// 密码都是 TLSPassword，库是 xone_tls
type TLSMySQL struct {
	Addr string
}

// DSN 用账号 user 连这个实例，不带 tls 参数（TLS 交给配置里的 TLS 块）
func (m *TLSMySQL) DSN(user string) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/xone_tls", user, TLSPassword, m.Addr)
}

// StartTLSMySQL 在临时目录里 --initialize-insecure 一个新实例、带着我们的证书起在随机端口上
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

	// 经 Unix socket 以 root（--initialize-insecure：没有密码）建账号和库。
	// require_secure_transport 不挡 socket：它本来就被当成安全的传输
	root, err := sql.Open("mysql", (&mysql.Config{User: "root", Net: "unix", Addr: sock, AllowNativePasswords: true}).FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, stmt := range []string{
		"CREATE DATABASE xone_tls",
		// 明文连过来时要让 require_secure_transport 说话（3159），账号上就不能再有别的拦法：
		// 实测 8.0.46 上 REQUIRE SSL 的账号走明文、默认的 caching_sha2_password 走明文，
		// 都是先回 1045 Access denied，看不出是传输不安全被拒。所以这个账号不加 REQUIRE、
		// 用 mysql_native_password
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

// TLSRedis 两个只开 TLS 端口的 redis-server（port 0）：Addr 不要客户端证书，
// MTLSAddr 要（tls-auth-clients yes）。密码都是 TLSPassword
type TLSRedis struct {
	Addr, MTLSAddr string
}

// StartTLSRedis 起两个 redis-server，测试结束时停掉。redis-server 没带 TLS 时 t.Skip
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
		// 密码经 stdin 上的配置交过去，不上命令行（ps 看得见命令行）
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

// SelfSignedCert 在 dir 里生成一张给 127.0.0.1 / localhost 的自签名证书，
// 返回证书、私钥文件的路径，和只信这张证书的 CertPool
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
