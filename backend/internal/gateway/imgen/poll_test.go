package imgen

import "testing"

func TestImagePollAttempts(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  int
	}{
		{name: "empty model", model: "", want: defaultImagePollAttempts},
		{name: "gpt image 1", model: "gpt-image-1", want: defaultImagePollAttempts},
		{name: "gpt image 1.5", model: "gpt-image-1.5", want: defaultImagePollAttempts},
		{name: "gpt image 2", model: "gpt-image-2", want: gptImage2PollAttempts},
		{name: "gpt image 2 variant", model: " GPT-IMAGE-2-high ", want: gptImage2PollAttempts},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imagePollAttempts(tc.model); got != tc.want {
				t.Fatalf("imagePollAttempts(%q) = %d, want %d", tc.model, got, tc.want)
			}
		})
	}
}
