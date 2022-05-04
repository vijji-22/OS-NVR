package headers

import (
	"testing"

	"nvr/pkg/video/gortsplib/pkg/base"

	"github.com/stretchr/testify/require"
)

var casesTransport = []struct {
	name string
	vin  base.HeaderValue
	vout base.HeaderValue
	h    Transport
}{
	{
		"tcp play request / response",
		base.HeaderValue{`RTP/AVP/TCP;interleaved=0-1`},
		base.HeaderValue{`RTP/AVP/TCP;interleaved=0-1`},
		Transport{
			Protocol:       TransportProtocolTCP,
			InterleavedIDs: &[2]int{0, 1},
		},
	},
}

func TestTransportRead(t *testing.T) {
	for _, ca := range casesTransport {
		t.Run(ca.name, func(t *testing.T) {
			var h Transport
			err := h.Read(ca.vin)
			require.NoError(t, err)
			require.Equal(t, ca.h, h)
		})
	}
}

func TestTransportReadErrors(t *testing.T) {
	for _, ca := range []struct {
		name string
		hv   base.HeaderValue
		err  string
	}{
		{
			"empty",
			base.HeaderValue{},
			"value not provided",
		},
		{
			"2 values",
			base.HeaderValue{"a", "b"},
			"value provided multiple times ([a b])",
		},
		{
			"invalid keys",
			base.HeaderValue{`key1="k`},
			"apexes not closed (key1=\"k)",
		},
		{
			"protocol not found",
			base.HeaderValue{`invalid;unicast;client_port=14186-14187`},
			"protocol not found (invalid;unicast;client_port=14186-14187)",
		},
		{
			"invalid interleaved port",
			base.HeaderValue{`RTP/AVP;unicast;interleaved=aa-14187`},
			"invalid ports (aa-14187)",
		},
		{
			"invalid ttl",
			base.HeaderValue{`RTP/AVP;unicast;ttl=aa`},
			"strconv.ParseUint: parsing \"aa\": invalid syntax",
		},
		{
			"invalid destination",
			base.HeaderValue{`RTP/AVP;unicast;destination=aa`},
			"invalid destination (aa)",
		},
		{
			"invalid ports 1",
			base.HeaderValue{`RTP/AVP;unicast;port=aa`},
			"invalid ports (aa)",
		},
		{
			"invalid ports 2",
			base.HeaderValue{`RTP/AVP;unicast;port=aa-bb-cc`},
			"invalid ports (aa-bb-cc)",
		},
		{
			"invalid ports 3",
			base.HeaderValue{`RTP/AVP;unicast;port=aa-14187`},
			"invalid ports (aa-14187)",
		},
		{
			"invalid ports 4",
			base.HeaderValue{`RTP/AVP;unicast;port=14186-aa`},
			"invalid ports (14186-aa)",
		},
		{
			"invalid client port",
			base.HeaderValue{`RTP/AVP;unicast;client_port=aa-14187`},
			"invalid ports (aa-14187)",
		},
		{
			"invalid server port",
			base.HeaderValue{`RTP/AVP;unicast;server_port=aa-14187`},
			"invalid ports (aa-14187)",
		},
		{
			"invalid ssrc 1",
			base.HeaderValue{`RTP/AVP;unicast;ssrc=zzz`},
			"encoding/hex: invalid byte: U+007A 'z'",
		},
		{
			"invalid ssrc 2",
			base.HeaderValue{`RTP/AVP;unicast;ssrc=030A0B0C0D0E0F`},
			"invalid SSRC",
		},
		{
			"invalid mode",
			base.HeaderValue{`RTP/AVP;unicast;mode=aa`},
			"invalid transport mode: 'aa'",
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			var h Transport
			err := h.Read(ca.hv)
			require.EqualError(t, err, ca.err)
		})
	}
}

func TestTransportWrite(t *testing.T) {
	for _, ca := range casesTransport {
		t.Run(ca.name, func(t *testing.T) {
			req := ca.h.Write()
			require.Equal(t, ca.vout, req)
		})
	}
}
