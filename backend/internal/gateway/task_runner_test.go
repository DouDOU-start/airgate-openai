package gateway

import "testing"

func TestSanitizeTaskMessage(t *testing.T) {
	cases := []struct {
		name string
		task *TaskError
		want string
	}{
		{
			name: "invalid request keeps raw message",
			task: &TaskError{Type: "invalid_request", Message: "模型不支持该尺寸"},
			want: "模型不支持该尺寸",
		},
		{
			name: "rate limited is generic",
			task: &TaskError{Type: "rate_limited", Message: "The usage limit has been reached"},
			want: "当前请求过多，请稍后重试",
		},
		{
			name: "auth error is generic",
			task: &TaskError{Type: "auth_error", Message: "token invalid"},
			want: "账号认证失败，请联系管理员",
		},
		{
			name: "upstream error is generic",
			task: &TaskError{Type: "upstream_error", Message: "server exploded"},
			want: "请求暂时无法完成，请稍后重试",
		},
		{
			name: "grpc desc is extracted",
			task: &TaskError{Type: "upstream_error", Message: "rpc error: code = Unknown desc = 原因详述"},
			want: "原因详述",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTaskMessage(tc.task); got != tc.want {
				t.Fatalf("sanitizeTaskMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}
