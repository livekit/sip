package sip

import (
	"strings"

	"github.com/pion/sdp/v3"
)

// AMR/AMR-WB SDP fmtp handling for answers (livekit/sip#747).
//
// media-sdk registers AMR/AMR-WB as bandwidth-efficient only (octet-align=0),
// and AnswerMedia emits rtpmap without echoing offer fmtp. Accepting an
// octet-aligned offer (or other unsupported modes) therefore produces a
// mismatched answer and garbled audio. RFC 4867 §8.3.1 requires the answerer
// to echo certain parameters for the accepted payload type (or reject it).
//
// Until media-sdk can negotiate octet-aligned RTP we:
//  1. strip unsupported AMR formats from the offer before answering, and
//  2. echo the required fmtp attributes for any AMR format we accept.

var amrAnswerUnsupported = map[string]func(string) bool{
	// Default is 0 (bandwidth-efficient). octet-align=1 needs a different
	// RTP payload format that media-sdk does not implement yet.
	"octet-align": func(v string) bool { return v == "1" },
	// CRC-protected frames are not implemented.
	"crc": func(v string) bool { return v == "1" },
	// Robust sorting is not implemented.
	"robust-sorting": func(v string) bool { return v == "1" },
	// Interleaving requires a different de-interleave path.
	"interleaving": func(v string) bool { return v != "" && v != "0" },
}

func isAMRName(name string) bool {
	n := strings.ToUpper(name)
	return n == "AMR" || n == "AMR-WB"
}

func parseFmtpParams(fmtp string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(fmtp, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			out[strings.ToLower(strings.TrimSpace(part))] = ""
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

func amrFmtpUnsupported(fmtp string) bool {
	if fmtp == "" {
		// No fmtp → octet-align defaults to 0 (bandwidth-efficient).
		return false
	}
	params := parseFmtpParams(fmtp)
	for key, bad := range amrAnswerUnsupported {
		if v, ok := params[key]; ok && bad(v) {
			return true
		}
	}
	return false
}

func amrFmtpForAnswer(offerFmtp string) string {
	if offerFmtp == "" || amrFmtpUnsupported(offerFmtp) {
		return ""
	}
	params := parseFmtpParams(offerFmtp)
	var parts []string
	// RFC 4867 §8.3.1: answerer MUST include these if present in the offer
	// for the accepted payload type (omit defaults).
	for _, key := range []string{
		"octet-align",
		"mode-change-capability",
		"max-red",
	} {
		v, ok := params[key]
		if !ok {
			continue
		}
		// Skip octet-align=0 (default).
		if key == "octet-align" && (v == "" || v == "0") {
			continue
		}
		parts = append(parts, key+"="+v)
	}
	// mode-set / mode-change-period / mode-change-neighbor may be answered
	// with a subset; echo offer values when present so both sides agree.
	for _, key := range []string{
		"mode-set",
		"mode-change-period",
		"mode-change-neighbor",
	} {
		if v, ok := params[key]; ok {
			parts = append(parts, key+"="+v)
		}
	}
	return strings.Join(parts, ";")
}

func attrPayloadType(value string) string {
	pt, _, ok := strings.Cut(value, " ")
	if !ok {
		return value
	}
	return pt
}

func rtpmapCodecName(value string) string {
	_, rest, ok := strings.Cut(value, " ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// filterAMROfferSDP removes AMR/AMR-WB dynamic formats whose fmtp we cannot
// answer correctly, returning a rewritten offer suitable for media-sdk Answer.
// Remaining formats (including bandwidth-efficient AMR) are left intact.
func filterAMROfferSDP(offerData []byte) ([]byte, error) {
	var offer sdp.SessionDescription
	if err := offer.Unmarshal(offerData); err != nil {
		return offerData, err
	}
	changed := filterOfferAMRFormats(&offer)
	if !changed {
		return offerData, nil
	}
	out, err := offer.Marshal()
	if err != nil {
		return offerData, err
	}
	return out, nil
}

// filterOfferAMRFormats removes unsupported AMR formats in-place.
// Returns true if the description was modified.
func filterOfferAMRFormats(offer *sdp.SessionDescription) bool {
	if offer == nil {
		return false
	}
	changed := false
	for _, md := range offer.MediaDescriptions {
		if md.MediaName.Media != "audio" {
			continue
		}
		dropPT := make(map[string]struct{})
		rtpmapNameByPT := make(map[string]string)
		fmtpByPT := make(map[string]string)
		for _, a := range md.Attributes {
			switch a.Key {
			case "rtpmap":
				pt := attrPayloadType(a.Value)
				rtpmapNameByPT[pt] = rtpmapCodecName(a.Value)
			case "fmtp":
				pt, rest, ok := strings.Cut(a.Value, " ")
				if ok {
					fmtpByPT[pt] = rest
				}
			}
		}
		for pt, name := range rtpmapNameByPT {
			if !isAMRName(name) {
				continue
			}
			if amrFmtpUnsupported(fmtpByPT[pt]) {
				dropPT[pt] = struct{}{}
			}
		}
		if len(dropPT) == 0 {
			continue
		}
		changed = true
		filtered := make([]string, 0, len(md.MediaName.Formats))
		for _, f := range md.MediaName.Formats {
			if _, drop := dropPT[f]; !drop {
				filtered = append(filtered, f)
			}
		}
		md.MediaName.Formats = filtered
		attrs := make([]sdp.Attribute, 0, len(md.Attributes))
		for _, a := range md.Attributes {
			switch a.Key {
			case "rtpmap", "fmtp":
				if _, drop := dropPT[attrPayloadType(a.Value)]; drop {
					continue
				}
			}
			attrs = append(attrs, a)
		}
		md.Attributes = attrs
	}
	return changed
}

// appendAMRFmtpToAnswer mutates answer SDP: for each accepted AMR payload type,
// copy the offer's answerable fmtp onto the answer (RFC 4867 §8.3.1).
func appendAMRFmtpToAnswer(answer *sdp.SessionDescription, offerData []byte) {
	if answer == nil {
		return
	}
	var offer sdp.SessionDescription
	if err := offer.Unmarshal(offerData); err != nil {
		return
	}

	offerFmtpByPT := map[string]string{}
	for _, md := range offer.MediaDescriptions {
		if md.MediaName.Media != "audio" {
			continue
		}
		rtpNames := map[string]string{}
		for _, a := range md.Attributes {
			if a.Key == "rtpmap" {
				rtpNames[attrPayloadType(a.Value)] = rtpmapCodecName(a.Value)
			}
		}
		for _, a := range md.Attributes {
			if a.Key != "fmtp" {
				continue
			}
			pt, rest, ok := strings.Cut(a.Value, " ")
			if !ok || !isAMRName(rtpNames[pt]) {
				continue
			}
			offerFmtpByPT[pt] = rest
		}
	}
	if len(offerFmtpByPT) == 0 {
		return
	}

	for _, md := range answer.MediaDescriptions {
		if md.MediaName.Media != "audio" {
			continue
		}
		haveFmtp := map[string]struct{}{}
		rtpNames := map[string]string{}
		for _, a := range md.Attributes {
			switch a.Key {
			case "fmtp":
				haveFmtp[attrPayloadType(a.Value)] = struct{}{}
			case "rtpmap":
				rtpNames[attrPayloadType(a.Value)] = rtpmapCodecName(a.Value)
			}
		}
		for pt, name := range rtpNames {
			if !isAMRName(name) {
				continue
			}
			if _, ok := haveFmtp[pt]; ok {
				continue
			}
			offerFmtp, ok := offerFmtpByPT[pt]
			if !ok {
				continue
			}
			echo := amrFmtpForAnswer(offerFmtp)
			if echo == "" {
				continue
			}
			md.Attributes = append(md.Attributes, sdp.Attribute{
				Key:   "fmtp",
				Value: pt + " " + echo,
			})
		}
	}
}
