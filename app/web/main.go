// cfddns-web —— 飞牛 fnOS 应用的常驻服务。
//
// 定位与边界（重要）：
//
//	本程序**不实现任何 DNS 同步逻辑**。所有与 Cloudflare 的交互都通过调用
//	官方 cfddns 二进制完成：
//	  · 触发同步  -> cfddns --config <file> --json
//	  · 查看状态  -> 读取同一份配置与 cfddns 自己写的 state.json
//	它只负责：提供配置读写接口、按间隔调用官方二进制、回显运行状态。
//	这样上游一旦更新，同步行为随之更新，本程序无需跟着改。
//
// 本应用**没有网页界面**：配置通过「应用设置」向导填写，或直接编辑
// etc/config.yaml。manifest 不声明 desktop_applaunchname，因此应用中心里
// 只提供「禁用 / 启动」。
//
// 只依赖 Go 标准库；监听 Unix Socket，由 fnOS 统一网关鉴权后转发。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	progVersion = "1.0.0"
	maxBody     = 1 << 20
	runTimeout  = 180 * time.Second
)

var (
	cfgPath   = flag.String("config", "", "配置文件路径")
	binPath   = flag.String("bin", "", "官方 cfddns 二进制路径")
	sockPath  = flag.String("socket", "", "Unix Socket 路径")
	addr      = flag.String("addr", "", "额外监听 TCP 地址（调试用）")
	interval  = flag.Duration("interval", 5*time.Minute, "自动同步间隔，0 表示关闭定时")
	showVer   = flag.Bool("version", false, "显示版本")
	devNoAuth = flag.Bool("dev-no-auth", false, "调试：允许回环来源跳过管理员校验")
)

// ---------------------------------------------------------------- 配置模型
//
// 字段与官方 cfddns 的配置格式一一对应，读写时保持同样的键名与顺序，
// 保证手工编辑与网页编辑产出的文件一致。

type Record struct {
	Name  string `json:"name"`
	Type  string `json:"type"`          // A / AAAA / both
	TTL   int    `json:"ttl,omitempty"` // 1=自动
	Proxy *bool  `json:"proxy,omitempty"`
}

type Family struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Source  string `json:"source,omitempty"`
}

type Config struct {
	Token   string   `json:"token"`
	Zone    string   `json:"zone"`
	Records []Record `json:"records"`
	IPv4    Family   `json:"ipv4"`
	IPv6    Family   `json:"ipv6"`
}

func (c *Config) normalize() {
	on := true
	if c.IPv4.Enabled == nil {
		c.IPv4.Enabled = &on
	}
	if c.IPv6.Enabled == nil {
		c.IPv6.Enabled = &on
	}
	if c.IPv4.Source == "" {
		c.IPv4.Source = "auto"
	}
	if c.IPv6.Source == "" {
		c.IPv6.Source = "auto"
	}
	for i := range c.Records {
		r := &c.Records[i]
		switch strings.ToUpper(strings.TrimSpace(r.Type)) {
		case "A":
			r.Type = "A"
		case "AAAA":
			r.Type = "AAAA"
		default:
			r.Type = "BOTH"
		}
		if r.TTL == 0 {
			r.TTL = 1
		}
	}
}

func (c *Config) enabled(fam string) bool {
	f := &c.IPv4
	if fam == "ipv6" {
		f = &c.IPv6
	}
	return f.Enabled == nil || *f.Enabled
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("请填写 Cloudflare API 令牌")
	}
	if strings.TrimSpace(c.Zone) == "" {
		return errors.New("请填写要绑定的域名")
	}
	if len(c.Records) == 0 {
		return errors.New("至少添加一条记录")
	}
	for _, r := range c.Records {
		if strings.TrimSpace(r.Name) == "" {
			return errors.New("存在记录名为空的条目")
		}
		switch r.Type {
		case "A", "AAAA", "BOTH":
		default:
			return fmt.Errorf("记录 %q 的类型无效", r.Name)
		}
	}
	if !c.enabled("ipv4") && !c.enabled("ipv6") {
		return errors.New("IPv4 与 IPv6 探测至少要启用一个")
	}
	return nil
}

// ---------------------------------------------------------------- YAML 读写
//
// 官方 cfddns 的配置是 YAML。这里只支持它实际会产出的那个子集，
// 目的不是做通用 YAML 库，而是保证「网页保存后官方二进制仍能读」。
// 遇到不认识的键会原样保留，避免把用户手写的注释与字段抹掉。

type yamlLine struct {
	indent int
	key    string
	val    string
	raw    string
	kind   string // kv / item / other
}

func parseConfig(data []byte) (*Config, []yamlLine, error) {
	cfg := &Config{}
	var lines []yamlLine
	section := ""

	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(raw)
		lines = append(lines, yamlLine{raw: raw})
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		body := strings.TrimLeft(raw, " ")

		if strings.HasPrefix(body, "- ") || body == "-" {
			rest := strings.TrimSpace(strings.TrimPrefix(body, "-"))
			if section == "records" {
				cfg.Records = append(cfg.Records, Record{TTL: 1, Type: "BOTH"})
				r := &cfg.Records[len(cfg.Records)-1]

				// 列表项有三种写法：
				//   - name: pc      完整写法的首行，值在后续缩进行
				//   - pc: AAAA      简写：冒号后必须是合法类型
				//   - pc            纯名字
				//
				// 判断顺序很关键：必须先排除保留字段名，否则 "- name: a"
				// 会被简写分支当成「名字叫 name、类型是 A」—— 因为 "a"
				// 恰好是 A 的合法别名，右侧像类型并不代表左侧就是记录名。
				if k, v, ok := splitKV(rest); ok && isRecordField(k) {
					if err := setRecordField(r, k, v); err != nil {
						return nil, lines, err
					}
				} else if name, typ, ok := splitShorthand(rest); ok {
					r.Name, r.Type = name, typ
				} else {
					r.Name = unquote(rest)
				}
			}
			continue
		}

		k, v, ok := splitKV(body)
		if !ok {
			continue
		}

		if indent == 0 {
			switch strings.ToLower(k) {
			case "token":
				cfg.Token = unquote(v)
				section = ""
			case "zone", "domain":
				cfg.Zone = unquote(v)
				section = ""
			case "records", "dns":
				// dns 是 records 的别名：上游 gen-config 生成的默认配置用的就是它，
				// 用户把那份配置拷进来时记录不能丢。
				section = "records"
				for _, part := range inlineList(v) {
					r := Record{TTL: 1, Type: "BOTH"}
					if name, typ, ok := splitShorthand(part); ok {
						r.Name, r.Type = name, typ
					} else {
						r.Name = unquote(part)
					}
					cfg.Records = append(cfg.Records, r)
				}
			case "ipv4", "ipv6":
				section = strings.ToLower(k)
			default:
				section = ""
			}
			continue
		}

		// 缩进层：属于 records 的字段，或 ipv4/ipv6 的开关
		if section == "records" && len(cfg.Records) > 0 && isRecordField(k) {
			if err := setRecordField(&cfg.Records[len(cfg.Records)-1], k, v); err != nil {
				return nil, lines, err
			}
		} else if section == "ipv4" || section == "ipv6" {
			f := &cfg.IPv4
			if section == "ipv6" {
				f = &cfg.IPv6
			}
			switch strings.ToLower(k) {
			case "enabled":
				b := strings.EqualFold(unquote(v), "true")
				f.Enabled = &b
			case "source":
				f.Source = unquote(v)
			}
		}
	}
	cfg.normalize()
	return cfg, lines, nil
}

func normType(v string) (string, bool) {
	switch strings.ToUpper(unquote(v)) {
	case "A":
		return "A", true
	case "AAAA":
		return "AAAA", true
	case "BOTH", "ALL", "":
		return "BOTH", true
	}
	return "", false
}

// isRecordField 判断键是否是记录的字段名。
// 用于区分「- name: pc」（完整写法的首行）与「- pc: AAAA」（简写）。
func isRecordField(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "name", "host", "type", "ttl", "proxy", "comment":
		return true
	}
	return false
}

// setRecordField 把 key/value 写入记录，供列表项首行与后续缩进行共用。
func setRecordField(r *Record, k, v string) error {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "name", "host":
		r.Name = unquote(v)
	case "type":
		if t, ok := normType(v); ok {
			r.Type = t
		}
	case "ttl":
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(unquote(v)), "%d", &n); err == nil {
			r.TTL = n
		}
	case "proxy":
		b := strings.EqualFold(unquote(v), "true")
		r.Proxy = &b
	}
	return nil
}

func splitKV(s string) (string, string, bool) {
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inD:
			inS = !inS
		case c == '"' && !inS:
			inD = !inD
		case c == ':' && !inS && !inD:
			if i+1 < len(s) && s[i+1] != ' ' && s[i+1] != '\t' {
				continue
			}
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

func splitShorthand(s string) (string, string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", "", false
	}
	name := unquote(strings.TrimSpace(s[:i]))
	if t, ok := normType(s[i+1:]); ok {
		return name, t, true
	}
	return "", "", false
}

func inlineList(v string) []string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[") {
		return nil
	}
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	var out []string
	for _, p := range strings.Split(v, ",") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	inS, inD := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#':
			if !inS && !inD && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
				s = strings.TrimSpace(s[:i])
				i = len(s)
			}
		}
	}
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			if s[0] == '"' {
				var out string
				if json.Unmarshal([]byte(s), &out) == nil {
					return out
				}
			}
			return s[1 : len(s)-1]
		}
	}
	return s
}

// renderConfig 按官方格式写出配置。保留一节说明性注释，便于用户手改时理解。
func renderConfig(c *Config) []byte {
	var b strings.Builder
	b.WriteString("# cfddns 配置（由飞牛应用页面生成，也可手工编辑）\n")
	b.WriteString("# 格式与命令行版 cfddns 完全一致。\n\n")
	b.WriteString("# Cloudflare API 令牌\n")
	fmt.Fprintf(&b, "token: %s\n\n", quoteYAML(c.Token))
	b.WriteString("# 要绑定的域名（zone ID 由程序自动查询，不用填）\n")
	fmt.Fprintf(&b, "zone: %s\n\n", quoteYAML(c.Zone))
	b.WriteString("# 要更新的记录：名字 或 名字: 类型（A / AAAA / both）\n")
	b.WriteString("#   pc 表示 pc.你的域名；@ 表示根域名；* 表示泛解析\n")
	b.WriteString("records:\n")
	for _, r := range c.Records {
		fmt.Fprintf(&b, "  - name: %s\n", quoteYAML(r.Name))
		fmt.Fprintf(&b, "    type: %s\n", strings.ToLower(r.Type))
		if r.TTL > 1 {
			fmt.Fprintf(&b, "    ttl: %d\n", r.TTL)
		}
		if r.Proxy != nil {
			fmt.Fprintf(&b, "    proxy: %v\n", *r.Proxy)
		}
	}
	b.WriteString("\n# 公网 IP 探测\n")
	b.WriteString("ipv4:\n")
	fmt.Fprintf(&b, "  enabled: %v\n", c.enabled("ipv4"))
	fmt.Fprintf(&b, "  source: %s\n", orDefault(c.IPv4.Source, "auto"))
	b.WriteString("ipv6:\n")
	fmt.Fprintf(&b, "  enabled: %v\n", c.enabled("ipv6"))
	fmt.Fprintf(&b, "  source: %s\n", orDefault(c.IPv6.Source, "auto"))
	return []byte(b.String())
}

func quoteYAML(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#'\"{}[],&*?|<>=!%@`") ||
		strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		b, _ := json.Marshal(s)
		return string(b)
	}
	return s
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// ---------------------------------------------------------------- 状态板

type runner struct {
	bin     string
	cfg     string
	mu      sync.Mutex // 串行化对官方二进制的调用，避免并发写同一份 state.json
	running bool
	last    *Snapshot
}

type Snapshot struct {
	At      string            `json:"at"`
	OK      bool              `json:"ok"`
	IPs     map[string]string `json:"ips,omitempty"`
	Error   string            `json:"error,omitempty"`
	Elapsed int64             `json:"elapsed_ms,omitempty"`
	Counts  map[string]int    `json:"counts,omitempty"`
	Results []RecordResult    `json:"results,omitempty"`
}

type RecordResult struct {
	FQDN    string `json:"fqdn"`
	Type    string `json:"type"`
	Want    string `json:"want"`
	Current string `json:"current"`
	Status  string `json:"status"`
	Error   string `json:"error"`
}

// runOnce 调用官方二进制执行一轮同步，并把它的 --json 输出转成网页用结构。
//
// 参数解析完全依赖上游：本程序不判断 IP、不拼接 API 请求，
// 只把官方输出里的字段搬运到界面上。
func (r *runner) runOnce(ctx context.Context, dryRun bool) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = true
	defer func() { r.running = false }()

	sn := Snapshot{At: time.Now().Format(time.RFC3339), IPs: map[string]string{}}
	args := []string{"--config", r.cfg, "--json"}
	if dryRun {
		args = append(args, "--dry-run")
	}

	start := time.Now()
	out, err := r.exec(ctx, args...)
	sn.Elapsed = time.Since(start).Milliseconds()

	// 官方在「有记录更新失败」时返回非零，这是正常语义，不能当成执行失败；
	// 因此只要输出能被解析成 JSON 就以它为准，仅当解析失败才报错。
	parsed := parseUpstreamJSON(out)
	if parsed == nil {
		msg := strings.TrimSpace(string(out))
		if err != nil && msg == "" {
			msg = err.Error()
		}
		if msg == "" {
			msg = "官方 cfddns 没有返回可解析的结果"
		}
		sn.OK = false
		sn.Error = clip(msg, 500)
		r.last = &sn
		return sn
	}

	sn.OK = true
	if raw, ok := parsed["counts"].(map[string]any); ok {
		sn.Counts = map[string]int{}
		for k, v := range raw {
			if n, ok := v.(float64); ok {
				sn.Counts[k] = int(n)
			}
		}
	}
	if raw, ok := parsed["results"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			res := RecordResult{
				FQDN:    str(m["fqdn"]),
				Type:    str(m["type"]),
				Want:    str(m["want"]),
				Current: str(m["current"]),
				Status:  str(m["status"]),
				Error:   str(m["error"]),
			}
			sn.Results = append(sn.Results, res)
		}
	}
	if sn.Counts != nil && sn.Counts["error"] > 0 {
		sn.Error = fmt.Sprintf("有 %d 条记录处理失败，详见下表", sn.Counts["error"])
	}
	r.last = &sn
	return sn
}

// exec 运行官方二进制，返回合并后的 stdout（stderr 一并带回用于报错）。
func (r *runner) exec(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return []byte(buf.String()), err
}

func parseUpstreamJSON(b []byte) map[string]any {
	s := strings.TrimSpace(string(b))
	// 官方输出是纯 JSON，但为了容忍前面可能的提示行，这里从第一个 '{' 开始截取
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil
	}
	if _, ok := m["results"]; !ok {
		return nil
	}
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------- HTTP 服务

type server struct {
	cfgFile string
	run     *runner
	mu      sync.Mutex
}

func (s *server) load() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.cfgFile)
	if err != nil {
		if os.IsNotExist(err) {
			c := &Config{}
			c.normalize()
			return c, nil
		}
		return nil, err
	}
	c, _, err := parseConfig(b)
	return c, err
}

func (s *server) save(c *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.cfgFile), 0o700); err != nil {
		return err
	}
	tmp := s.cfgFile + ".tmp"
	if err := os.WriteFile(tmp, renderConfig(c), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfgFile)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": progVersion})
	})

	// 只读诊断：把网关注入的身份头原样回显，便于判断「保存被拒」的原因。
	// 仅暴露 X-Trim-* / X-Forwarded-For / X-Real-IP，不含任何凭据。
	mux.HandleFunc("/api/whoami", func(w http.ResponseWriter, r *http.Request) {
		id := gatewayIdentity(r)
		writeJSON(w, 200, map[string]any{
			"ok":          true,
			"version":     progVersion,
			"admin":       isAdminRequest(r),
			"identity":    id,
			"remote_addr": r.RemoteAddr,
			"path":        r.URL.Path,
			"can_write":   isAdminRequest(r),
			"hint":        "若 admin 为 false，保存配置会被拒绝；请把本页内容反馈给开发者",
		})
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			c, err := s.load()
			if err != nil {
				writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true, "config": c})
		case http.MethodPut, http.MethodPost:
			if !requireAdmin(w, r) {
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
			if err != nil {
				writeJSON(w, 400, map[string]any{"ok": false, "error": "读取请求失败"})
				return
			}
			var c Config
			if err := json.Unmarshal(body, &c); err != nil {
				writeJSON(w, 400, map[string]any{"ok": false, "error": "请求不是合法 JSON"})
				return
			}
			c.Token = strings.TrimSpace(c.Token)
			c.Zone = strings.TrimSpace(c.Zone)
			c.normalize()
			if err := c.validate(); err != nil {
				writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			if err := s.save(&c); err != nil {
				writeJSON(w, 500, map[string]any{"ok": false, "error": "保存配置失败: " + err.Error()})
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true})
		default:
			writeJSON(w, 405, map[string]any{"ok": false, "error": "方法不支持"})
		}
	})

	// 纯文本状态页：应用入口打开的就是这里。
	//
	// 本应用不提供网页管理界面（配置通过应用设置向导填写，或直接编辑
	// etc/config.yaml）。但应用需要有可打开的入口才会显示在系统的应用列表里，
	// 因此以这个返回 JSON 的端点作为入口，打开即可看到运行状态。
	mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		c, err := s.load()
		out := map[string]any{
			"ok":           true,
			"version":      progVersion,
			"service":      "cfddns 动态域名",
			"usage":        "配置通过「应用设置」向导填写，或直接编辑 etc/config.yaml 后重启应用",
			"request_host": r.Host,
		}
		if err != nil {
			out["ok"] = false
			out["error"] = err.Error()
			writeJSON(w, 200, out)
			return
		}
		out["configured"] = c.Token != "" && c.Zone != "" && len(c.Records) > 0
		out["zone"] = c.Zone
		out["ipv4_enabled"] = c.enabled("ipv4")
		out["ipv6_enabled"] = c.enabled("ipv6")
		var recs []string
		for _, rec := range c.Records {
			recs = append(recs, rec.Name+" ("+strings.ToLower(rec.Type)+")")
		}
		out["records"] = recs

		s.run.mu.Lock()
		last := s.run.last
		busy := s.run.running
		s.run.mu.Unlock()
		out["busy"] = busy
		if last != nil {
			out["last_run_at"] = last.At
			out["last_run_ok"] = last.OK
			out["last_run_error"] = last.Error
			out["last_run_ms"] = last.Elapsed
			if last.Counts != nil {
				out["counts"] = last.Counts
			}
		}
		writeJSON(w, 200, out)
	})

	// 状态：优先返回本次进程内最后一次运行结果；没有则回退到官方写的 state.json
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		c, _ := s.load()
		out := map[string]any{
			"ok": true, "version": progVersion,
			"configured": c != nil && c.Token != "" && c.Zone != "" && len(c.Records) > 0,
		}
		s.run.mu.Lock()
		last := s.run.last
		busy := s.run.running
		s.run.mu.Unlock()
		if last != nil {
			out["last"] = last
		}
		out["busy"] = busy
		writeJSON(w, 200, out)
	})

	// 立即同步：调用官方二进制
	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, 405, map[string]any{"ok": false, "error": "请用 POST"})
			return
		}
		if !requireAdmin(w, r) {
			return
		}
		c, err := s.load()
		if err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := c.validate(); err != nil {
			writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), runTimeout)
		defer cancel()
		sn := s.run.runOnce(ctx, false)
		writeJSON(w, 200, map[string]any{"ok": sn.OK, "last": sn})
	})

	mux.Handle("/", rootHandler())
	return stripGatewayPrefix(mux)
}

const gatewayPrefix = "/app/cfddns"

// stripGatewayPrefix 让服务对「带前缀」与「不带前缀」两种转发方式都能工作。
func stripGatewayPrefix(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == gatewayPrefix || strings.HasPrefix(r.URL.Path, gatewayPrefix+"/") {
			p := strings.TrimPrefix(r.URL.Path, gatewayPrefix)
			if p == "" {
				p = "/"
			}
			r.URL.Path = p
		}
		h.ServeHTTP(w, r)
	})
}

// adminHeaderNames 是可能承载「是否管理员」的请求头。
//
// fnOS 统一网关在转发时会注入身份头。文档里写的是 X-Trim-Isadmin，
// 但实际取值形式（true / 1 / yes）与是否真的注入并无保证，而一旦判定失败，
// 用户在页面上看到的现象只是「点保存没反应」，很难定位。
// 这里放宽取值形式，并在拒绝时把收到的相关头部写进日志与响应，便于排查。
var adminHeaderNames = []string{"X-Trim-Isadmin", "X-Trim-IsAdmin", "X-Trim-Admin"}

func isAdminRequest(r *http.Request) bool {
	for _, name := range adminHeaderNames {
		v := strings.TrimSpace(r.Header.Get(name))
		if v == "" {
			continue
		}
		switch strings.ToLower(v) {
		case "true", "1", "yes", "y", "on":
			return true
		}
	}
	return false
}

// gatewayIdentity 返回便于排查的请求头信息。
//
// 优先列出 X-Trim-* 与转发相关头；若一个都没匹配到，则回显全部头名 ——
// 因为无法预知 fnOS 网关用什么头名传递身份，只看到空集合就无从判断
// 「保存被拒」是权限问题还是头名不匹配。
// 凭据类头（Authorization / Cookie 等）的值一律不回显。
func gatewayIdentity(r *http.Request) map[string]string {
	out := map[string]string{}
	matched := false
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-trim-") || lk == "x-forwarded-for" || lk == "x-real-ip" {
			out[k] = strings.Join(v, ",")
			matched = true
		}
	}
	if !matched {
		for k := range r.Header {
			lk := strings.ToLower(k)
			if lk == "authorization" || lk == "cookie" || lk == "proxy-authorization" {
				out[k] = "(已隐藏)"
				continue
			}
			out[k] = "(值已省略)"
		}
	}
	return out
}

// requireAdmin 只信任 fnOS 网关注入的管理员头。
// 不把「来源是环回」当作管理员凭据：经 Unix Socket 转发时对端地址并非
// 127.0.0.1，据此放行会变成越权。
func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if isAdminRequest(r) {
		return true
	}
	if *devNoAuth {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			if ip, perr := netip.ParseAddr(host); perr == nil && ip.IsLoopback() {
				log.Printf("警告: --dev-no-auth 生效，已跳过管理员校验")
				return true
			}
		}
	}
	id := gatewayIdentity(r)
	log.Printf("拒绝 %s %s：未识别到管理员身份。收到的网关头部: %v", r.Method, r.URL.Path, id)
	writeJSON(w, 403, map[string]any{
		"ok":       false,
		"error":    "当前账号没有管理员权限，或网关未注入管理员标识头",
		"identity": id,
	})
	return false
}

// 根路径：本应用没有网页界面，因此返回 JSON 而不是 HTML。
//
// 保留这个响应只是为了让直接访问应用根路径时有明确结果（说明本应用无常驻
// 网页界面并列出可用接口），不承担任何界面职责 —— 应用中心里也不会出现
// 「打开」按钮，因为 manifest 未声明 desktop_applaunchname。
func rootHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, 405, map[string]any{"ok": false, "error": "方法不支持"})
			return
		}
		if p := path.Clean("/" + r.URL.Path); p != "/" && p != "/index.html" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{
			"ok":      true,
			"version": progVersion,
			"service": "cfddns 动态域名",
			"note":    "本应用没有网页界面；配置请用「应用设置」向导或编辑 etc/config.yaml",
			"endpoints": []string{
				"api/summary   运行状态（是否已配置、记录列表、最近一次同步结果）",
				"api/config    读取当前配置（GET）或保存配置（PUT，需管理员）",
				"api/status    同步状态",
				"api/run       立即同步一次（POST，需管理员）",
				"api/whoami    查看当前请求携带的身份信息",
				"api/health    健康检查",
			},
		})
	})
}

// ---------------------------------------------------------------- 入口

// schedule 按固定间隔调用官方二进制执行同步。
//
// 定时放在本进程而不是再起一个 --interval 常驻的官方进程，是为了避免
// 两个进程写同一份 state.json（官方由 --config 推导状态文件位置，
// 同目录两个实例会互相覆盖缓存）。
func (s *server) schedule(stop <-chan struct{}) {
	run := func() {
		c, err := s.load()
		if err != nil || c.validate() != nil {
			return // 未配置完整时安静跳过，页面会提示用户去填
		}
		ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
		defer cancel()
		sn := s.run.runOnce(ctx, false)
		if sn.OK {
			log.Printf("定时同步完成，耗时 %dms", sn.Elapsed)
		} else {
			log.Printf("定时同步未成功: %s", sn.Error)
		}
	}

	go run() // 启动即预跑一次，页面打开就有数据

	if *interval <= 0 {
		log.Printf("定时同步已关闭（--interval=0）")
		return
	}
	log.Printf("定时同步：每 %s 一次", *interval)
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			go run()
		}
	}
}

func main() {
	flag.Parse()
	if *showVer {
		fmt.Printf("cfddns-web %s\n", progVersion)
		return
	}

	if *cfgPath == "" || *binPath == "" {
		log.Fatal("必须同时指定 --config 与 --bin")
	}
	sp := *sockPath
	if sp == "" && *addr == "" {
		log.Fatal("必须指定 --socket 或 --addr")
	}

	srv := &server{
		cfgFile: *cfgPath,
		run:     &runner{bin: *binPath, cfg: *cfgPath},
	}

	stop := make(chan struct{})
	go srv.schedule(stop)

	var listeners []net.Listener

	if sp != "" {
		if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
			log.Fatalf("无法创建 Socket 目录: %v", err)
		}
		if fi, err := os.Lstat(sp); err == nil {
			if fi.Mode()&os.ModeSocket != 0 {
				_ = os.Remove(sp) // 上次异常退出留下的陈旧 socket
			} else {
				log.Fatalf("路径 %s 已存在且不是 socket", sp)
			}
		}
		ln, err := net.Listen("unix", sp)
		if err != nil {
			log.Fatalf("监听 Unix Socket 失败: %v", err)
		}
		_ = os.Chmod(sp, 0o660)
		listeners = append(listeners, ln)
		log.Printf("cfddns-web %s 已启动，Socket=%s 配置=%s 二进制=%s",
			progVersion, sp, *cfgPath, *binPath)
	}
	if *addr != "" {
		tl, err := net.Listen("tcp", *addr)
		if err != nil {
			log.Fatalf("监听 %s 失败: %v", *addr, err)
		}
		listeners = append(listeners, tl)
		log.Printf("额外监听 TCP %s（仅建议绑定 127.0.0.1）", *addr)
	}

	httpSrv := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	for _, l := range listeners {
		go func(l net.Listener) {
			if err := httpSrv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("服务退出: %v", err)
			}
		}(l)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在停止…")
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	if sp != "" {
		_ = os.Remove(sp)
	}
	log.Printf("已停止")
}
