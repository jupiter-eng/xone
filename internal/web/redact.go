package web

// 请求/响应里的敏感信息脱敏。
//
// 这里的每一条判断都必须「拿不准就当作敏感」。脱敏逻辑的失败模式是不对称的：
// 多遮一个字段只是少一点排查信息，漏遮一个就是密码进了日志、而且没人会发现。

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Redacted 被遮掉的值统一写成这个
const Redacted = "***REDACTED***"

// 默认的敏感词。
//
// 按「含」匹配，不是按「等于」：比较前双方都去掉大小写和分隔符（_ - . 空格），
// 于是 password 能认出 new_password、old-password、user.password，
// token 能认出 sessionToken、X-Csrf-Token。原先是精确匹配，这些全都原样进了日志。
//
// 词要挑得够「专」：auth 会误中 author，key 会误中 primary_key、Idempotency-Key，
// pwd 这样的短词会在随机串里撞上、把整段纯文本 body 遮掉，所以这里写的是
// apikey、authorization 这样的整词。在这个范围之内，拿不准的词宁可放进来：
// 误遮只是少一点排查信息，漏遮就是凭证落盘。
var defaultWords = []string{
	"password", "passwd", "secret", "token", "authorization",
	"apikey", "accesskey", "privatekey", "credential", "cookie", "session", "signature",
}

// 默认按名字精确遮掉的请求头。
//
// 它们的名字里本来就带着敏感词，词表已经能认出来；单独列一遍是为了
// 不让它们的安危系于词表——哪天有人把词表收窄了，这几个仍然是遮的。
var defaultHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Auth-Token",
}

// 进程里只有这一张词表和一张名单：各 Web 集成公开的 AddSensitiveFields /
// AddSensitiveHeaders（xgin/middleware）都转到这里。每个集成各存一份的话，
// 使用者在 xgin 那边补的词，换一个框架的服务就不认了。
//
// 两张表都是写时复制：追加时整张换一份新的，已经交出去的那份从此不再改动，
// 读的一侧拿到之后不用再持锁。
var (
	mu         sync.RWMutex
	fieldWords = normalizeAll(defaultWords) // 规范化之后的敏感词：默认词表 + AddSensitiveFields
	headerSet  = lowerSet(defaultHeaders)   // 小写的请求头名单：默认名单 + AddSensitiveHeaders
)

// AddSensitiveFields 追加敏感词，大小写与分隔符不敏感。
//
// 与默认词表同一条规则：字段名里含这个词就遮，body 和请求头都算。
// 比如加了 id_card，idCard、user_id_card、X-Id-Card 都会被遮掉。
func AddSensitiveFields(fields ...string) {
	mu.Lock()
	defer mu.Unlock()
	// Clip 之后 append 一定另起一个底层数组，不会写进别人手上那份
	fieldWords = append(slices.Clip(fieldWords), normalizeAll(fields)...)
}

// AddSensitiveHeaders 追加按名字精确遮掉的请求头，大小写不敏感。
//
// 用于名字里没有敏感词、内容却是凭证的头，比如 X-Tenant-Id。
// 名字里带敏感词的头不需要加，默认就遮。
func AddSensitiveHeaders(headers ...string) {
	mu.Lock()
	defer mu.Unlock()
	set := maps.Clone(headerSet)
	for _, h := range headers {
		set[strings.ToLower(h)] = true
	}
	headerSet = set
}

// words 取规范化之后的敏感词。拿到的那份不会再变，只读
func words() []string {
	mu.RLock()
	defer mu.RUnlock()
	return fieldWords
}

// headers 取小写的请求头名单。拿到的那份不会再变，只读
func headers() map[string]bool {
	mu.RLock()
	defer mu.RUnlock()
	return headerSet
}

// normalizeAll 逐个规范化，丢掉规范化之后为空的（全是分隔符的词会匹配一切）
func normalizeAll(fields []string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if n := normalize(f); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// lowerSet 转成小写的集合
func lowerSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, h := range names {
		set[strings.ToLower(h)] = true
	}
	return set
}

// isSeparator 比较字段名时忽略的字符
func isSeparator(c byte) bool { return c == '_' || c == '-' || c == '.' || c == ' ' }

// normalize 做大小写折叠并去掉分隔符：New_Password、new-password、newPassword
// 规范化之后都是 newpassword
func normalize(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r >= utf8.RuneSelf || !isSeparator(byte(r)) {
			b.WriteRune(foldRune(r))
		}
	}
	return b.String()
}

// foldRune 取 r 所在大小写折叠等价类的代表，两个字符折叠后相等，
// 当且仅当 encoding/json 认为它们是同一个字符。
//
// 不能只处理 ASCII：encoding/json 匹配字段名用的是 Unicode 折叠，
// 实测 {"ſecret":...}（长 s，U+017F）绑得上 Secret，
// {"toKen":...}（开尔文符号，U+212A）绑得上 Token。只转 ASCII 小写的话，
// 这两个 key 在词表里认不出来，body 走快路径原样进日志，
// 而服务端读到的明明就是那个字段。
//
// 代表取等价类里最小的那个字符（encoding/json 内部也是这么取的），
// 是大写 ASCII 字母的话再转成小写，好让规范化之后的词表仍然是读得懂的样子
func foldRune(r rune) rune {
	if r >= utf8.RuneSelf {
		// SimpleFold 在等价类里按升序轮转，第一次不升反降的那个就是最小的
		next := unicode.SimpleFold(r)
		for next > r {
			r, next = next, unicode.SimpleFold(next)
		}
		r = next
	}
	if 'A' <= r && r <= 'Z' {
		r += 'a' - 'A'
	}
	return r
}

// sensitive 判断字段名是否敏感：规范化之后含任一敏感词。
// body 预检也用它，把整段 body 当作一个「字段名」，两处因此只有一份规则。
//
// 代价量过：五个请求头的 RedactHeaders 从精确匹配时的 780ns/7 allocs
// 变成 1460ns/11 allocs（每个头规范化一次）。试过逐字节手写扫描来省掉
// 这次分配，反而慢得多——strings.Contains 比手写循环快得多。body 预检
// 原先就是那样写的：每个字节位置对每个词各比一次，非 ASCII 的位置每次都要
// 解码、折叠。230 字节、带几个汉字的 JSON，RedactBody 要 23µs，
// 接近上限的 256KB 要 13ms（纯 ASCII）/ 46ms（带中文）；换成规范化一次
// 再 Contains 之后是 2.4µs、2.5ms / 2.9ms，代价是多一份 body 大小的分配。
func sensitive(name string, ws []string) bool {
	n := normalize(name)
	return slices.ContainsFunc(ws, func(w string) bool { return strings.Contains(n, w) })
}

// newlines 把换行去掉，免得一条日志被撑成好几行
//
// 只写单字节的 \r 和 \n：\r\n 逐个字节删掉结果一样。写成单字节，
// Replacer 走的是逐字节的实现，没有换行时原样返回、不分配；
// 多写一个 \r\n 就退回通用实现，每次都要拷两遍
var newlines = strings.NewReplacer("\r", "", "\n", "")

// RedactBody 按 Content-Type 脱敏请求体
//
// Content-Type 先转小写：这个头按 RFC 9110 是大小写不敏感的，
// 照字面比的话 "Application/JSON" 就走不进 JSON 那一支。
//
// JSON 的判断是「含 json」而不是精确匹配 application/json：
// application/vnd.api+json、application/problem+json、text/json
// 都是 JSON，精确匹配会把它们整个漏过去。认错了也不会更糟——
// 解不出 JSON 的那一支本来就是整个遮掉。
//
// 别的类型（text/plain、没带 Content-Type 的）本身是合法 JSON 的，也按 JSON 遮：
// gin 的 ShouldBindJSON 不看 Content-Type，这样的 body 服务端照样按 JSON 读。
// 走纯文本那一支的话只做字面扫描，键名用 \u 转义写的 password 认不出来，密码原样进日志
func RedactBody(body []byte, contentType string) string {
	if len(body) == 0 {
		return ""
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "json"):
		return redactJSON(body)
	case strings.Contains(ct, "x-www-form-urlencoded"):
		return redactForm(string(body))
	case isJSON(body):
		return redactJSON(body)
	default:
		return redactOpaque(body)
	}
}

// isJSON body 是不是一个 JSON 对象、数组或字符串，见 RedactBody。
//
// 先看第一个非空白字符再 json.Valid：纯文本 body 解不开时 json.Valid 要造一个 SyntaxError，
// 实测一段 56 字节的纯文本从 2 次分配变成 6 次、慢了约 300ns。数字、true 这些标量里也没有可遮的东西
func isJSON(body []byte) bool {
	b := bytes.TrimLeft(body, " \t\r\n")
	return len(b) > 0 && (b[0] == '{' || b[0] == '[' || b[0] == '"') && json.Valid(b)
}

// RedactText 脱敏一段没有结构的文本，比如 handler 返回的错误、panic 的值。三条规矩：
//
//   - 出现敏感词就整段遮掉，同纯文本 body："login failed for user=x password=y"
//     定位不了是哪一段，只能整段不要；
//   - 没有敏感词时，遮掉 URL 和 MySQL DSN 里 userinfo 的密码，其余原样留着：
//     驱动报错最爱带整串 DSN——dial postgres://app:pw@db:5432/prod、
//     app:pw@tcp(db:3306)/prod——里面一个敏感词都没有，只看词表的话原样进日志；
//   - 换行换成 "; "、末尾的换行去掉：gin 的 c.Errors.String() 一条错误一行，
//     像 body 那样直接删掉换行，两条错误就粘成了一句。
//
// 密码的写法和 XONE_DEBUG 打印配置时一样（internal/config 的 redactString）：
// 留着用户名和主机，只把密码换掉，一眼看得出连的是哪个库、用的哪个账号。
// 换成的是本包的 Redacted，好让一行访问日志里遮掉的东西都是同一个标记
func RedactText(s string) string {
	if s == "" {
		return ""
	}
	if sensitive(s, words()) {
		return Redacted
	}
	return textNewlines.Replace(strings.TrimRight(redactCredentials(s), "\r\n"))
}

// redactCredentials 只遮 URL 和 MySQL DSN 里 userinfo 的密码，别的一概不动：RedactText 的第二条规矩。
// 请求头的值只过这一条，不套敏感词整段遮掉的那条——Vary: Cookie、
// Access-Control-Allow-Headers: Authorization 里的词是头名，不是凭证，敏感的头名由名单和词表按名字遮。
//
// 两种 DSN 的写法都离不开 @：没有 @ 就不跑正则。请求头、JSON 的字符串值每个都要过这里，
// 绝大多数不带 @，省下的是两次正则扫描和一次拷贝
func redactCredentials(s string) string {
	if strings.IndexByte(s, '@') < 0 {
		return s
	}
	s = urlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@")
	return mysqlUserinfo.ReplaceAllString(s, "$1:"+Redacted+"@$2")
}

// 文本里夹着的凭证。规则同 internal/config 的 urlUserinfo / mysqlDSN，只是不锚在开头：
// 那边是一个配置值就是一整串 DSN，这里是一句话里的某一段。
//
// 密码按最后一个 @ 算（贪婪匹配再回退），与 net/url、go-sql-driver/mysql 解析 DSN 时一致：
// 密码里没转义的 @ 不会在日志里留下半截。代价是偶尔多遮一点，拿不准就当作敏感
var (
	urlUserinfo   = regexp.MustCompile(`(://[^:/?#@\s]*):[^/\s]+@`)        // postgres://u:p@h、redis://:p@h
	mysqlUserinfo = regexp.MustCompile(`([^\s:@/]+):\S+@((?:tcp|unix)\()`) // u:p@tcp(h:3306)/db、u:p@unix(/sock)/db
)

// textNewlines RedactText 用的换行处理：\n 换成 "; "，\r 删掉（\r\n 因此也是一个 "; "）。
// 与 newlines 一样只写单字节的键，走的是逐字节的实现
var textNewlines = strings.NewReplacer("\r", "", "\n", "; ")

// redactOpaque 处理认不出结构的 body（text/plain、xml、没带 Content-Type 的……）
//
// 定位不了具体字段，但「里面有没有敏感字段名」是看得出来的。有就整个遮掉。
// 这与 JSON 那一支解析失败时的处理是同一条规矩：定位不了就不能放行。
//
// 这里只做字面扫描，不像 JSON 那样见到反斜杠就一律当作可疑：
// 反斜杠是 JSON 的转义语法，纯文本里它就是个普通字符，
// 一条带 Windows 路径的日志不该因此被整段遮掉。
func redactOpaque(body []byte) string {
	s := string(body)
	if sensitive(s, words()) {
		return Redacted
	}
	// 没有敏感字段名，但可能带着 URL / DSN 凭证（postgres://u:p@h），同 RedactText
	return newlines.Replace(redactCredentials(s))
}

// redactJSON 解析 JSON 并遮掉敏感字段，字符串值另外过一遍 RedactText（值里夹着的整串 DSN）
//
// 结构上的局限：只认键名。[{"name":"password","value":"x"}] 这种「名值对」的数组，
// 敏感的是 name 的值、密码在 value 里，键名里都没有敏感词——name 的值因为含敏感词被 RedactText 遮掉，
// value 的 x 原样留着。这种形状的 body 要遮就用 AddSensitiveFields 把 value 这样的键加进词表，或者别记 body
//
// 解码用 UseNumber、编码关掉 HTML 转义：默认的 any 会把数字解成 float64，
// 12345678901234567890 重新写出来成了 12345678901234567000；
// 默认的编码器又把 < > & 写成反斜杠 u 开头的转义序列。两样都让日志里的值跟请求里的对不上。
func redactJSON(body []byte) string {
	ws := words()
	s := string(body)
	// 有 @ 也得解析：值里可能夹着 DSN（postgres://u:p@h、u:p@tcp(h)），见 RedactText
	if !mayContainField(s, ws) && strings.IndexByte(s, '@') < 0 {
		return newlines.Replace(s)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var data any
	if err := dec.Decode(&data); err != nil || dec.Decode(new(any)) != io.EOF {
		// 解析不了（或者后面还跟着别的东西）就整个遮掉。这里不能原样返回：
		// 能走到这一步说明字面扫描认为里面有敏感字段名，只是我们没能力定位它
		return Redacted
	}

	data = redactValue(data, ws)
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return Redacted
	}
	return strings.TrimSuffix(out.String(), "\n") // Encode 总在末尾补一个换行
}

// mayContainField 判断 body 里是否可能出现敏感字段名。
//
// 只在「确定不含」时返回 false——它的作用是跳过「解析 + 遍历 + 重新序列化」
// 这条重路径，而不是做安全判断，所以拿不准一律返回 true。
//
// 反斜杠是硬性的分水岭：JSON 允许在字符串里写 \uXXXX，于是
//
//	{"password": "hunter2"}
//
// 的原始字节里根本没有 password 这个子串。上一版只做字面扫描，
// 这种 body 会走快路径原样进日志——密码明文落盘，而且不会有任何迹象。
// 所以只要出现反斜杠就交给解析器，由它把转义还原成真实的 key。
//
// 字面扫描用的就是字段名比对的那个 sensitive：同一种折叠、同样忽略分隔符，
// api_key 对得上 apikey，{"ſecret":...} 对得上 secret。
func mayContainField(body string, ws []string) bool {
	return strings.IndexByte(body, '\\') >= 0 || sensitive(body, ws)
}

// redactValue 键名敏感的整个遮掉，字符串值过一遍 RedactText，返回遮过的 v（map 和切片就地改）
func redactValue(v any, ws []string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitive(k, ws) {
				t[k] = Redacted
				continue
			}
			t[k] = redactValue(val, ws)
		}
	case []any:
		for i, item := range t {
			t[i] = redactValue(item, ws)
		}
	case string:
		return RedactText(t)
	}
	return v
}

// redactForm 脱敏 form-urlencoded 请求体
//
// 用 url.ParseQuery 而不是按 & 和 = 切：表单的键是百分号编码的，
//
//	p%61ssword=hunter2
//
// 按字面切出来的键是 p%61ssword，跟 password 对不上，于是照样进日志。
// 解析一遍再比，才是跟服务端实际读到的键同一个东西。
func redactForm(body string) string {
	ws := words()

	values, err := url.ParseQuery(body)
	if err != nil {
		// 解析不了就整个遮掉，理由同 JSON：定位不了就不能放行
		return Redacted
	}
	for k, vs := range values {
		if sensitive(k, ws) {
			values[k] = []string{Redacted}
			continue
		}
		// 值里的 URL / DSN 凭证照样遮，同 JSON 那一支对字符串值调的 RedactText：
		// ?next=https://u:p@host 或者表单里带一个 DSN，不该因为换了编码就原样进日志
		for i, v := range vs {
			vs[i] = redactCredentials(v)
		}
	}
	// Encode 会把遮掉的值也转义成 %2A%2A%2AREDACTED%2A%2A%2A，日志里一眼认不出；
	// 标记里没有 & 和 =，还原回来不会让键值对的边界变得有歧义
	return strings.ReplaceAll(values.Encode(), url.QueryEscape(Redacted), Redacted)
}

// RedactHeaders 脱敏请求头，返回一个可以直接交给 slog 的值。
//
// 返回 slog.Value 而不是序列化好的字符串：交出字符串的话，slog 还要把它
// 当成一个普通字符串字段再转义一遍，日志里就成了
//
//	"请求头":"{\"Authorization\":[\"***REDACTED***\"]}"
//
// 检索时得先解一层字符串才能解 JSON。交出 slog.Value，序列化只发生一次，
// 这个字段在日志里就是一个正常的嵌套对象，顺带省掉了那次 json.Marshal
// （实测整条日志从 2777ns/19 allocs 降到 1630ns/9 allocs）。
//
// 多值的头拼成逗号分隔的一个字符串，而不是数组：同一个字段名在不同日志行里
// 忽而是字符串忽而是数组，日志系统建索引时会直接拒收。
//
// 两条规则，满足一条就遮：名字在名单里（默认名单 + AddSensitiveHeaders），
// 或者名字里带敏感词（与 body 字段同一张词表）。名单永远列不全——
// Proxy-Authorization 就曾经漏在外面——所以词表是兜底的那一层。
func RedactHeaders(h http.Header) slog.Value {
	set := headers()
	ws := words()

	// 按 key 排序：map 的遍历顺序是随机的，不排的话同样一组请求头
	// 每行日志的字段顺序都不一样，对不上也没法 diff
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	attrs := make([]slog.Attr, 0, len(keys))
	for _, k := range keys {
		name := strings.ToLower(k) // 两张名单都按小写查，转一次就够了
		if set[name] || sensitive(k, ws) {
			attrs = append(attrs, slog.String(k, Redacted))
			continue
		}
		v := strings.Join(h[k], ", ") // 单值时原样返回、不分配，绝大多数头都是单值
		if urlHeaders[name] {
			v = stripURL(v)
		}
		// 名字里没有敏感词的头，值里照样可能夹着整串 DSN：只遮其中的密码（redactCredentials）。
		// 不套 RedactText 的敏感词规则：Vary: Cookie、Access-Control-Allow-Headers: Authorization
		// 里的词是头名不是凭证，整段遮掉的话头日志就没法看了。
		// 没有 @ 的值原样返回、不分配：五个请求头的 RedactHeaders 实测仍是 11 allocs、约 1.9µs
		attrs = append(attrs, slog.String(k, redactCredentials(v)))
	}
	return slog.GroupValue(attrs...)
}

// urlHeaders 值是一个 URL 的请求头和响应头，记日志时去掉查询串、片段和 userinfo。
//
// 访问日志的 path 特意不带查询串（凭证常在那里：?token=、OAuth 回调的 ?code=），
// 而 Referer 带的正是上一个页面的完整 URL，照抄的话等于从侧门又记了一遍。
// x- 开头的几个是反向代理转发原始请求 URI 用的，内容就是这次请求的 path 加查询串。
// 后三个是响应头：OAuth 回调的 302 Location 带着 ?code=，隐式授权带着 #access_token=；
// Refresh 的值是「秒数; url=…」，截断的规则对它一样适用。
// Origin 按规范只有协议、主机和端口，不在此列
var urlHeaders = map[string]bool{
	"referer":          true,
	"x-original-url":   true,
	"x-original-uri":   true,
	"x-rewrite-url":    true,
	"x-forwarded-uri":  true,
	"location":         true,
	"content-location": true,
	"refresh":          true,
}

// stripURL 去掉第一个 ? 或 # 之后的全部内容，再去掉 :// 之后、主机之前的 userinfo（https://tok@host）。
//
// 按字符截断而不是 url.Parse：解析失败时还得决定怎么办，截断没有失败这回事。
// 多值拼在一起时会连后面的值一起去掉——多遮一点，不会漏。
// userinfo 整个去掉而不是只遮密码：只写用户名的那种（https://<token>@host）用户名本身就是凭证
func stripURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.Index(u, "://"); i >= 0 {
		host := u[i+3:]
		if j := strings.IndexByte(host, '/'); j >= 0 {
			host = host[:j]
		}
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			u = u[:i+3] + u[i+3+at+1:]
		}
	}
	return u
}
