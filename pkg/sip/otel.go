package sip

import (
	"runtime/debug"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func appendVersionAttr(out []attribute.KeyValue, m *debug.Module) []attribute.KeyValue {
	switch m.Path {
	case "github.com/livekit/sip":
		out = append(out, attribute.String(
			"livekit.sip.version", m.Version,
		))
	case "github.com/livekit/sipgo":
		out = append(out, attribute.String(
			"livekit.sipgo.proto.version", m.Version,
		))
	case "github.com/emiago/sipgo":
		vers := m.Version
		if m.Replace != nil {
			vers = m.Replace.Version
		}
		out = append(out, attribute.String(
			"livekit.sipgo.parser.version", vers,
		))
	}
	return out
}

func getSIPVersions() []attribute.KeyValue {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	var out []attribute.KeyValue
	out = appendVersionAttr(out, &info.Main)
	for _, d := range info.Deps {
		out = appendVersionAttr(out, d)
	}
	return out
}

var Tracer = otel.Tracer(
	"github.com/livekit/sip",
	trace.WithInstrumentationAttributes(getSIPVersions()...),
)
