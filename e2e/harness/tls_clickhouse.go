package harness

// TLS 的 e2e 里的 ClickHouse：另起一个只给这组用例用的容器（xone-ch 一个配置都不动），
// 开 tcp_port_secure 和 https_port，证书是 NewTLSCerts 现造的那一套，测试结束时删掉容器。
//
// 同一个容器里明文端口（tcp_port / http_port）也开着：「TLS 块开着却连到明文端口」
// 「不开 TLS 块却连到 TLS 端口」都要在真的服务端上量。
//
// 没有 docker、或者 xone-ch 用的那个镜像不在本机时 t.Skip，并说清缺的是什么——
// 不在测试里拉镜像：拉一次要几分钟，而且 CI 里没有 ClickHouse。

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

// TLSCHImage TLS 用的 ClickHouse 容器的镜像，与 xone-ch 同一个
func TLSCHImage() string {
	return env("XONE_E2E_CH_IMAGE", "mirror.gcr.io/clickhouse/clickhouse-server:24.8")
}

// TLSCHUser / TLSCHDB TLS 的 ClickHouse 上用密码登录的账号和库。密码是 TLSPassword
const (
	TLSCHUser = "xone"
	TLSCHDB   = "xone_tls"
)

// TLSClickHouse 一个开了 TLS 端口的 ClickHouse 容器（host 网络，只听 127.0.0.1）
//
// 账号 xone 用密码 TLSPassword 登录；账号 xone_mtls 没有密码，靠出示 CN=xone_mtls、
// 由正确的 CA 签的客户端证书登录（ssl_certificates）。服务端 verificationMode 是 relaxed：
// 请求客户端证书但不强求，带了就按 CA 校验。
type TLSClickHouse struct {
	Name string // 容器名

	NativeTLS string // tcp_port_secure
	HTTPS     string // https_port
	Native    string // tcp_port（明文）
	HTTP      string // http_port（明文）
}

// DSN 用 scheme（clickhouse / https / http）连 addr，账号 xone，不带任何 TLS 参数（TLS 交给配置里的 TLS 块）
func (c *TLSClickHouse) DSN(scheme, addr string) string {
	return fmt.Sprintf("%s://%s:%s@%s/%s", scheme, TLSCHUser, TLSPassword, addr, TLSCHDB)
}

// MTLSDSN 用账号 xone_mtls（证书认证，没有密码）连 addr
func (c *TLSClickHouse) MTLSDSN(scheme, addr string) string {
	return fmt.Sprintf("%s://%s@%s/%s", scheme, TLSMTLSUser, addr, TLSCHDB)
}

// StartTLSClickHouse 起容器、等四个端口都能连上，测试结束时 docker rm -f 掉
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
		// 容器里的 clickhouse 账号（uid 101）要读得到；目录只在测试期间存在
		copyFile(t, f.from, filepath.Join(dir, f.to), 0o644)
	}

	c := &TLSClickHouse{Name: "xone-ch-tls-" + NewID()}
	nativeTLS, https, native, http := FreePort(t), FreePort(t), FreePort(t), FreePort(t)
	addr := func(p int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(p)) }
	c.NativeTLS, c.HTTPS, c.Native, c.HTTP = addr(nativeTLS), addr(https), addr(native), addr(http)

	// 盖掉镜像自带的 docker_related_config.xml（它听 0.0.0.0 / ::）：只听 127.0.0.1，
	// 端口全换成空闲的，host 网络下与 xone-ch 的 9000 / 8123 / 9004 / 9005 / 9009 不撞
	listen := `<clickhouse><listen_host>127.0.0.1</listen_host></clickhouse>`
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

	// 密码经 --env-file 交过去，不上命令行（ps 看得见命令行）
	envFile := filepath.Join(dir, "env")
	envText := "CLICKHOUSE_USER=" + TLSCHUser + "\nCLICKHOUSE_PASSWORD=" + TLSPassword + "\n"
	if err := os.WriteFile(envFile, []byte(envText), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "-d", "--name", c.Name, "--network", "host", "--env-file", envFile,
		"--tmpfs", "/var/lib/clickhouse",
		"-v", dir + "/listen.xml:/etc/clickhouse-server/config.d/docker_related_config.xml:ro",
		"-v", dir + "/xone-tls.xml:/etc/clickhouse-server/config.d/xone-tls.xml:ro",
		"-v", dir + "/mtls-user.xml:/etc/clickhouse-server/users.d/xone-mtls.xml:ro",
		"-v", dir + ":/xone-tls:ro",
		TLSCHImage()}
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
	// 库自己建，不用 CLICKHOUSE_DB：入口脚本建库时调的 clickhouse-client 不带 --port，
	// host 网络下连到的是 127.0.0.1:9000 上的 xone-ch（实测报 516，容器随即退出）。
	// 经容器里的 clickhouse-client 连这个容器的明文端口，密码走环境变量，不上命令行
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

// Log 容器的服务端日志里含 needle 的行（最多 limit 行）
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
