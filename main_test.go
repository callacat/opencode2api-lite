package main

import "testing"

// TestCompareVersion 覆盖上游版本门槛判定：UA 里的 opencode/<version> 低于
// minOCVersion 时上游返回 426，比较语义错了会在运行期才暴露，这里钉住。
func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.18.31", "1.17.0", 1},
		{"1.17.0", "1.17.0", 0},
		{"1.17", "1.17.0", 0}, // 缺失段按 0 补齐
		{"1.16.9", "1.17.0", -1},
		{"2.0.0", "1.99.99", 1},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNormalizeOCVersion(t *testing.T) {
	if got := normalizeOCVersion("1.16.0"); got != minOCVersion {
		t.Errorf("低于门槛的版本应抬升到 %s，得到 %s", minOCVersion, got)
	}
	if got := normalizeOCVersion("1.18.31"); got != "1.18.31" {
		t.Errorf("达标版本应原样返回，得到 %s", got)
	}
}
