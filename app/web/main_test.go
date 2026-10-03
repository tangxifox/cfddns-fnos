package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 后端写出的配置必须能被自己读回，且与官方 cfddns 的解析结果一致。
// 这里用「写入 → 读回 → 再写入」的往返测试锁住格式兼容性。
func TestConfigRoundTrip(t *testing.T) {
	in := &Config{
		Token: "tok_test",
		Zone:  "example.com",
		Records: []Record{
			{Name: "a1", Type: "A", TTL: 1},
			{Name: "a2", Type: "AAAA", TTL: 1},
			{Name: "a3", Type: "BOTH", TTL: 1},
			{Name: "@", Type: "BOTH", TTL: 1},
			{Name: "*", Type: "A", TTL: 1},
		},
	}
	in.normalize()

	text := renderConfig(in)
	t.Logf("写出的配置:\n%s", text)

	got, _, err := parseConfig(text)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Zone != "example.com" || got.Token != "tok_test" {
		t.Errorf("顶层字段读回错误: zone=%q token=%q", got.Zone, got.Token)
	}
	if len(got.Records) != len(in.Records) {
		t.Fatalf("记录数 %d，期望 %d：%+v", len(got.Records), len(in.Records), got.Records)
	}
	for i := range in.Records {
		if got.Records[i].Name != in.Records[i].Name {
			t.Errorf("第 %d 条 name = %q，期望 %q", i, got.Records[i].Name, in.Records[i].Name)
		}
		if got.Records[i].Type != in.Records[i].Type {
			t.Errorf("第 %d 条 type = %q，期望 %q", i, got.Records[i].Type, in.Records[i].Type)
		}
	}

	// 二次往返必须稳定（幂等），否则页面每保存一次就污染一次
	text2 := renderConfig(got)
	if string(text2) != string(text) {
		t.Errorf("二次写出与首次不一致:\n--- 1 ---\n%s\n--- 2 ---\n%s", text, text2)
	}
}

// 官方 cfddns 的默认配置用 dns: 作为 records 的别名，必须能读
func TestParseUpstreamStyle(t *testing.T) {
	raw := `# cfddns 配置
token: "abc"
zone: example.com

dns:
  - pc: both
  - nas: AAAA

ipv4:
  enabled: true
  source: auto
`
	c, _, err := parseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.Zone != "example.com" || c.Token != "abc" {
		t.Errorf("顶层解析错误: zone=%q token=%q", c.Zone, c.Token)
	}
	// dns: 是 records 的别名，记录不能丢
	if len(c.Records) != 2 {
		t.Fatalf("应读到 2 条记录，实际 %d: %+v", len(c.Records), c.Records)
	}
	if c.Records[0].Name != "pc" || c.Records[0].Type != "BOTH" {
		t.Errorf("第 1 条解析错误: %+v", c.Records[0])
	}
	if c.Records[1].Name != "nas" || c.Records[1].Type != "AAAA" {
		t.Errorf("第 2 条解析错误: %+v", c.Records[1])
	}
}

func TestParseRecordsForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string // name:type
	}{
		{
			"完整写法",
			"token: t\nzone: e.com\nrecords:\n  - name: a\n    type: A\n  - name: b\n    type: AAAA\n",
			[]string{"a:A", "b:AAAA"},
		},
		{
			"简写",
			"token: t\nzone: e.com\nrecords:\n  - a: A\n  - b: both\n",
			[]string{"a:A", "b:BOTH"},
		},
		{
			"纯名字",
			"token: t\nzone: e.com\nrecords:\n  - a\n  - b\n",
			[]string{"a:BOTH", "b:BOTH"},
		},
		{
			"行内列表",
			"token: t\nzone: e.com\nrecords: [a, b: AAAA]\n",
			[]string{"a:BOTH", "b:AAAA"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, err := parseConfig([]byte(tc.raw))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			var got []string
			for _, r := range c.Records {
				got = append(got, r.Name+":"+r.Type)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("得到 %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	on := true
	base := func() *Config {
		return &Config{
			Token:   "t",
			Zone:    "e.com",
			Records: []Record{{Name: "a", Type: "A", TTL: 1}},
			IPv4:    Family{Enabled: &on},
		}
	}
	if err := base().validate(); err != nil {
		t.Errorf("合法配置不应报错: %v", err)
	}
	c := base()
	c.Token = ""
	if err := c.validate(); err == nil {
		t.Errorf("空令牌应报错")
	}
	c = base()
	c.Records = nil
	if err := c.validate(); err == nil {
		t.Errorf("空记录应报错")
	}
	off := false
	c = base()
	c.IPv4 = Family{Enabled: &off}
	c.IPv6 = Family{Enabled: &off}
	if err := c.validate(); err == nil {
		t.Errorf("两个协议都关应报错")
	}
}

func TestParseUpstreamJSON(t *testing.T) {
	good := `{"counts":{"updated":1,"unchanged":0,"error":0},"results":[{"fqdn":"a.example.com","type":"A","want":"1.2.3.4","status":"updated"}]}`
	m := parseUpstreamJSON([]byte(good))
	if m == nil {
		t.Fatalf("应能解析官方 JSON 输出")
	}
	if _, ok := m["results"]; !ok {
		t.Errorf("缺少 results 字段")
	}

	// 前面混入提示行也要能解析
	mixed := "同步中…\n" + good
	if parseUpstreamJSON([]byte(mixed)) == nil {
		t.Errorf("带前置提示行时应仍能解析")
	}
	// 非 JSON 输出应返回 nil，由调用方报错
	if parseUpstreamJSON([]byte("cfddns: 配置读取失败")) != nil {
		t.Errorf("非 JSON 输出不应被解析成结果")
	}
}

// 网关注入的管理员头形式可能不同，取值形式也要放宽；
// 一旦误判为「非管理员」，用户看到的现象只是「点保存没反应」。
func TestIsAdminRequest(t *testing.T) {
	cases := []struct {
		header string
		value  string
		want   bool
	}{
		{"X-Trim-Isadmin", "true", true},
		{"X-Trim-Isadmin", "TRUE", true},
		{"X-Trim-Isadmin", "1", true},
		{"X-Trim-Isadmin", "yes", true},
		{"X-Trim-Isadmin", "false", false},
		{"X-Trim-Isadmin", "0", false},
		{"X-Trim-Isadmin", "", false},
		{"X-Trim-IsAdmin", "true", true},
		{"X-Trim-Admin", "true", true},
		{"X-Trim-Username", "admin", false}, // 普通身份头不能当作管理员凭据
	}
	for _, tc := range cases {
		r := httptest.NewRequest("PUT", "/api/config", nil)
		r.Header.Set(tc.header, tc.value)
		if got := isAdminRequest(r); got != tc.want {
			t.Errorf("%s=%q -> %v，期望 %v", tc.header, tc.value, got, tc.want)
		}
	}
}

func TestGatewayIdentityRedactsOthers(t *testing.T) {
	r := httptest.NewRequest("PUT", "/api/config", nil)
	r.Header.Set("X-Trim-Username", "someone")
	r.Header.Set("X-Trim-Isadmin", "true")
	r.Header.Set("Authorization", "Bearer secret-token")
	r.Header.Set("Cookie", "session=abc")
	id := gatewayIdentity(r)
	if _, ok := id["X-Trim-Username"]; !ok {
		t.Errorf("应包含 X-Trim-Username: %v", id)
	}
	for k := range id {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "authorization") || strings.Contains(lk, "cookie") {
			t.Errorf("不应回显敏感头: %v", id)
		}
	}
}
