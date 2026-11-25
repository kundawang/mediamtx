package conf

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPathMaxInboundBytesPerSec(t *testing.T) {
	for _, ca := range []struct {
		name string
		conf Path
		want int64
		ok   bool
	}{
		{
			name: "unconfigured",
			conf: Path{},
			want: 0,
			ok:   false,
		},
		{
			name: "explicit bytes per second",
			conf: Path{MaxInboundBytesPerSecond: 12345},
			want: 12345,
			ok:   true,
		},
		{
			name: "bits per second are converted to bytes",
			conf: Path{MaxInboundBitrate: 800_000},
			want: 100_000,
			ok:   true,
		},
		{
			name: "publish bitrate alias",
			conf: Path{MaxPublishBitrate: 800},
			want: 100,
			ok:   true,
		},
		{
			name: "generic maxBitrate alias",
			conf: Path{MaxBitrate: 800},
			want: 100,
			ok:   true,
		},
		{
			name: "publish bytes alias",
			conf: Path{MaxPublishBytesPerSecond: 777},
			want: 777,
			ok:   true,
		},
		{
			name: "most permissive alias wins",
			conf: Path{MaxInboundBitrate: 800, MaxInboundBytesPerSecond: 500},
			want: 500,
			ok:   true,
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			got, ok := ca.conf.MaxInboundBytesPerSec()
			require.Equal(t, ca.ok, ok)
			require.Equal(t, ca.want, got)
		})
	}
}
