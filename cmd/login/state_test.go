// state_test.go --state 参数与 state 落盘自愈的契约测试。
//
// 背景：panel 以子进程驱动 login，固定 temp 路径上的残留文件（只读属性/提权
// ACL）曾让 os.WriteFile 直接 Access is denied，授权链接拿不到、前端无法添加
// 账号。writeStateFile 的「删旧重试」与 --state 注入是这条链路的修复。
package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestExtractStateFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     string
		wantRest []string
		wantErr  bool
	}{
		{"absent", []string{"url"}, "", []string{"url"}, false},
		{"eq-form", []string{"--state=C:\\x\\s.json", "url"}, "C:\\x\\s.json", []string{"url"}, false},
		{"sep-form", []string{"--state", "/tmp/s.json", "poll"}, "/tmp/s.json", []string{"poll"}, false},
		{"with-realm", []string{"--realm=global", "--state=/s.json", "url"}, "/s.json", []string{"--realm=global", "url"}, false},
		{"empty-value", []string{"--state=", "url"}, "", nil, true},
		{"missing-value", []string{"--state"}, "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, err := extractStateFlag(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got state=%q rest=%v", got, rest)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want || !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("got (%q, %v) want (%q, %v)", got, rest, tc.want, tc.wantRest)
			}
		})
	}
}

func TestWriteStateFileHealsStaleReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wb2api-login-state.json")
	if err := os.WriteFile(path, []byte(`{"state":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 只读属性复现「Access is denied」：Windows 上 0444 落 FILE_ATTRIBUTE_READONLY，
	// POSIX 上是去掉写位；两者都会让 O_TRUNC 直写失败，走删旧重试路径。
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := writeStateFile(path, []byte(`{"state":"new","realm":"global"}`)); err != nil {
		t.Fatalf("write state: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"state":"new","realm":"global"}` {
		t.Errorf("state = %q, want overwritten content", raw)
	}
}
