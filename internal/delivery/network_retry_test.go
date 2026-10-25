package delivery

import (
	"errors"
	"testing"
)

// TestIsTransientNetworkError 钉住「重试的边界」：传输层抖动要重试，
// 认证/权限/分支保护必须原样暴露，否则一次凭据配置错误会变成三次无谓等待，
// 还会把真实原因埋进"推送分支失败"里。
func TestIsTransientNetworkError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{
			name: "schannel 握手被重置",
			msg:  "git push origin x 失败: fatal: unable to access 'https://github.com/o/r.git/': schannel: failed to receive handshake, SSL/TLS connection failed",
			want: true,
		},
		{
			name: "连接被对端中断",
			msg:  "fatal: unable to access 'https://github.com/o/r.git/': Failure when receiving data from the peer",
			want: true,
		},
		{
			name: "DNS 暂时失败",
			msg:  "fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com",
			want: true,
		},
		{
			name: "远端提前断开",
			msg:  "RPC failed; curl 56 OpenSSL SSL_read: Connection reset by peer",
			want: true,
		},
		{
			name: "认证失败不重试",
			msg:  "fatal: Authentication failed for 'https://github.com/o/r.git/'",
			want: false,
		},
		{
			name: "权限不足不重试",
			msg:  "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403",
			want: false,
		},
		{
			name: "仓库不存在不重试",
			msg:  "fatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 404",
			want: false,
		},
		{
			name: "非快进不重试",
			msg:  "! [rejected] main -> main (non-fast-forward)",
			want: false,
		},
		{
			name: "空错误",
			msg:  "",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			if c.msg != "" {
				err = errors.New(c.msg)
			}
			if got := IsTransientNetworkError(err); got != c.want {
				t.Fatalf("IsTransientNetworkError(%q) = %v, want %v", c.msg, got, c.want)
			}
		})
	}
}
