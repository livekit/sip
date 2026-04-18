package sip

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/icholy/digest"
	"github.com/pkg/errors"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/utils/guid"
	"github.com/livekit/psrpc"
	"github.com/livekit/sipgo/sip"
)

const (
	defaultRegisterExpiration = 300 * time.Second
	defaultRegisterRefresh    = 30 * time.Second
)

type RegistrationProfile struct {
	ProfileName                    string
	AutoMatchHosts                 []string
	Enabled                        bool
	InviteOnRegisterFailure        bool
	AlwaysRefreshBeforeInvite      bool
	RegistrarURI                   string
	AuthURI                        string
	AORUser                        string
	AuthUsername                   string
	ContactUser                    string
	FromDomain                     string
	Transport                      Transport
	Expires                        time.Duration
	RefreshBefore                  time.Duration
	IncludePortInAuthURI           bool
	IncludeTransportParamInAuthURI bool
}

type ResolvedRegistrationProfile struct {
	RegistrationProfile
	Registrar URI
	AuthURI   string
}

type registrationState struct {
	expiresAt time.Time
	inflight  chan struct{}
	err       error
}

type RegistrationManager struct {
	mu     sync.Mutex
	states map[string]*registrationState
}

func NewRegistrationManager() *RegistrationManager {
	return &RegistrationManager{
		states: make(map[string]*registrationState),
	}
}

func (m *RegistrationManager) ensure(ctx context.Context, c *Client, profile *ResolvedRegistrationProfile, password string, contact URI) error {
	if m == nil || profile == nil {
		return nil
	}

	key := profile.cacheKey()
	for {
		m.mu.Lock()
		st := m.states[key]
		if st == nil {
			st = &registrationState{}
			m.states[key] = st
		}
		if !profile.AlwaysRefreshBeforeInvite && st.inflight == nil && st.expiresAt.After(time.Now().Add(profile.RefreshBefore)) {
			m.mu.Unlock()
			return nil
		}
		if st.inflight != nil {
			wait := st.inflight
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wait:
				continue
			}
		}
		st.inflight = make(chan struct{})
		m.mu.Unlock()

		err := c.register(ctx, profile, password, contact)

		m.mu.Lock()
		if err == nil {
			st.expiresAt = time.Now().Add(profile.Expires)
		} else {
			st.expiresAt = time.Time{}
		}
		st.err = err
		close(st.inflight)
		st.inflight = nil
		m.mu.Unlock()
		return err
	}
}

func (p *ResolvedRegistrationProfile) cacheKey() string {
	if p == nil {
		return ""
	}
	return strings.Join([]string{
		p.ProfileName,
		p.Registrar.GetDest(),
		string(p.Transport),
		p.AuthUsername,
		p.AORUser,
		p.ContactUser,
		p.FromDomain,
	}, "|")
}

func defaultRegistrationProfiles() []RegistrationProfile {
	return []RegistrationProfile{
		{
			ProfileName:               "uis",
			AutoMatchHosts:            []string{"pbx.uiscom.ru"},
			Enabled:                   true,
			InviteOnRegisterFailure:   true,
			AlwaysRefreshBeforeInvite: true,
			RegistrarURI:              "sip:pbx.uiscom.ru",
			FromDomain:                "pbx.uiscom.ru",
			Expires:                   defaultRegisterExpiration,
			RefreshBefore:             defaultRegisterRefresh,
		},
		{
			ProfileName:             "sipuni",
			AutoMatchHosts:          []string{"sipuni.com", "voip.sipuni.com", "ni.ru"},
			Enabled:                 true,
			InviteOnRegisterFailure: true,
			Expires:                 120 * time.Second,
			RefreshBefore:           30 * time.Second,
		},
		{
			ProfileName:             "generic",
			Enabled:                 false,
			InviteOnRegisterFailure: true,
			Expires:                 defaultRegisterExpiration,
			RefreshBefore:           defaultRegisterRefresh,
		},
	}
}

func (c *Client) ensureRegistered(ctx context.Context, sipConf sipOutboundConfig) (*ResolvedRegistrationProfile, error) {
	if c == nil || c.sipCli == nil || c.sconf == nil {
		return nil, nil
	}
	if sipConf.user == "" || sipConf.pass == "" || sipConf.address == "" {
		return nil, nil
	}

	profile, err := resolveRegistrationProfile(sipConf)
	if err != nil || profile == nil {
		return profile, err
	}
	if !profile.Enabled {
		return profile, nil
	}

	contact := c.ContactURI(profile.Transport)
	contact.User = profile.ContactUser
	return profile, c.registrationManager.ensure(ctx, c, profile, sipConf.pass, contact)
}

func resolveRegistrationProfile(sipConf sipOutboundConfig) (*ResolvedRegistrationProfile, error) {
	addressHost := hostFromAddress(sipConf.address)
	profiles := defaultRegistrationProfiles()
	selected := profiles[len(profiles)-1]

	if overrideName := strings.TrimSpace(sipConf.featureFlags[registrationProfileFeatureFlag]); overrideName != "" {
		found := false
		for _, p := range profiles {
			if p.ProfileName == overrideName {
				selected = p
				found = true
				break
			}
		}
		if !found {
			selected = RegistrationProfile{ProfileName: overrideName}
		}
	} else {
		for _, p := range profiles {
			if p.ProfileName == "generic" {
				continue
			}
			if profileMatchesHost(p, addressHost) {
				selected = p
				break
			}
		}
	}

	overrideRegistrationProfile(&selected, sipConf.featureFlags)
	if selected.Transport == "" {
		selected.Transport = TransportFrom(sipConf.transport)
	}
	if selected.Transport == "" {
		selected.Transport = TransportUDP
	}
	if selected.Expires <= 0 {
		selected.Expires = defaultRegisterExpiration
	}
	if selected.RefreshBefore <= 0 || selected.RefreshBefore >= selected.Expires {
		selected.RefreshBefore = minDuration(defaultRegisterRefresh, selected.Expires/2)
	}
	if selected.AORUser == "" {
		selected.AORUser = sipConf.user
	}
	if selected.AuthUsername == "" {
		selected.AuthUsername = sipConf.user
	}
	if selected.ContactUser == "" {
		selected.ContactUser = sipConf.user
	}
	if selected.FromDomain == "" {
		if domain := hostFromRawURI(selected.RegistrarURI); domain != "" {
			selected.FromDomain = domain
		} else {
			selected.FromDomain = addressHost
		}
	}
	if selected.RegistrarURI == "" && addressHost != "" {
		selected.RegistrarURI = buildSIPURI(addressHost, selected.Transport, true, true)
	}
	if selected.AuthURI == "" {
		selected.AuthURI = buildSIPURI(hostFromRawURI(selected.RegistrarURI), selected.Transport, selected.IncludePortInAuthURI, selected.IncludeTransportParamInAuthURI)
	}

	registrar, err := parseRegistrationURI(selected.RegistrarURI, selected.Transport)
	if err != nil {
		return nil, err
	}
	if selected.AuthURI == "" {
		selected.AuthURI = normalizeRegisterAuthURI(registrar, selected.IncludePortInAuthURI, selected.IncludeTransportParamInAuthURI)
	}

	return &ResolvedRegistrationProfile{
		RegistrationProfile: selected,
		Registrar:           registrar,
		AuthURI:             selected.AuthURI,
	}, nil
}

func profileMatchesHost(profile RegistrationProfile, host string) bool {
	host = strings.ToLower(host)
	for _, candidate := range profile.AutoMatchHosts {
		candidate = strings.ToLower(candidate)
		if host == candidate || strings.HasSuffix(host, "."+candidate) {
			return true
		}
	}
	return false
}

func overrideRegistrationProfile(profile *RegistrationProfile, flags map[string]string) {
	if profile == nil {
		return
	}
	if v, ok := flags[registrationEnabledFeatureFlag]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			profile.Enabled = b
		}
	}
	if v, ok := flags[registrationRegistrarFeatureFlag]; ok && v != "" {
		profile.RegistrarURI = v
	}
	if v, ok := flags[registrationAuthURIFeatureFlag]; ok && v != "" {
		profile.AuthURI = v
	}
	if v, ok := flags[registrationAORUserFeatureFlag]; ok && v != "" {
		profile.AORUser = v
	}
	if v, ok := flags[registrationAuthUserFeatureFlag]; ok && v != "" {
		profile.AuthUsername = v
	}
	if v, ok := flags[registrationContactUserFeatureFlag]; ok && v != "" {
		profile.ContactUser = v
	}
	if v, ok := flags[registrationFromDomainFeatureFlag]; ok && v != "" {
		profile.FromDomain = v
	}
	if v, ok := flags[registrationTransportFeatureFlag]; ok && v != "" {
		profile.Transport = Transport(strings.ToLower(v))
	}
	if v, ok := flags[registrationExpiresSecFeatureFlag]; ok && v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			profile.Expires = time.Duration(sec) * time.Second
		}
	}
	if v, ok := flags[registrationIncludePortFeatureFlag]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			profile.IncludePortInAuthURI = b
		}
	}
	if v, ok := flags[registrationIncludeTransportFeatureFlag]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			profile.IncludeTransportParamInAuthURI = b
		}
	}
	if v, ok := flags[registrationInviteFallbackFeatureFlag]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			profile.InviteOnRegisterFailure = b
		}
	}
	if v, ok := flags[registrationAlwaysRefreshFeatureFlag]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			profile.AlwaysRefreshBeforeInvite = b
		}
	}
}

func (c *Client) register(ctx context.Context, profile *ResolvedRegistrationProfile, password string, contact URI) error {
	callID := sip.CallIDHeader(guid.New("reg_"))
	fromTag := sip.GenerateTagN(16)
	authHeaderName := ""
	authHeaderValue := ""

	for attempt := 0; attempt < 5; attempt++ {
		req := c.newRegisterRequest(profile, contact, fromTag, uint32(attempt+1), callID, authHeaderName, authHeaderValue)
		tx, err := c.sipCli.TransactionRequest(req)
		if err != nil {
			return err
		}

		resp, err := sipResponse(ctx, tx, c.closing.Watch(), nil)
		tx.Terminate()
		if err != nil {
			return err
		}

		switch resp.StatusCode {
		case sip.StatusOK:
			return nil
		case sip.StatusUnauthorized:
			authHeaderName = "Authorization"
		case sip.StatusProxyAuthRequired:
			authHeaderName = "Proxy-Authorization"
		case sip.StatusForbidden, sip.StatusNotFound, sip.StatusMethodNotAllowed, sip.StatusNotImplemented:
			return fmt.Errorf("REGISTER failed: %w", &livekit.SIPStatus{
				Code:   livekit.SIPStatusCode(resp.StatusCode),
				Status: resp.Reason,
			})
		default:
			return fmt.Errorf("REGISTER failed: %w", &livekit.SIPStatus{
				Code:   livekit.SIPStatusCode(resp.StatusCode),
				Status: resp.Reason,
			})
		}

		challengeHeader := resp.GetHeader(stringsForAuthHeader(authHeaderName))
		if challengeHeader == nil {
			return psrpc.NewError(psrpc.FailedPrecondition, errors.New("no auth header in sip register response"))
		}
		challenge, err := digest.ParseChallenge(challengeHeader.Value())
		if err != nil {
			return fmt.Errorf("invalid register challenge %q: %w", challengeHeader.Value(), err)
		}
		cred, err := digest.Digest(challenge, digest.Options{
			Method:   sip.REGISTER.String(),
			URI:      profile.AuthURI,
			Username: profile.AuthUsername,
			Password: password,
		})
		if err != nil {
			return err
		}
		authHeaderValue = cred.String()
	}

	return psrpc.NewError(psrpc.FailedPrecondition, fmt.Errorf("max auth retry attempts reached for SIP register"))
}

func (c *Client) newRegisterRequest(profile *ResolvedRegistrationProfile, contact URI, fromTag string, cseq uint32, callID sip.CallIDHeader, authHeaderName, authHeaderValue string) *sip.Request {
	registerURI := profile.Registrar.GetURI()
	aorURI := &sip.Uri{
		Scheme: "sip",
		User:   profile.AORUser,
		Host:   profile.FromDomain,
	}
	contactURI := *contact.GetContactURI()
	maxForwards := sip.MaxForwardsHeader(70)

	req := sip.NewRequest(sip.REGISTER, *registerURI)
	req.SetDestination(profile.Registrar.GetDest())
	req.AppendHeader(&sip.ToHeader{Address: *aorURI})
	req.AppendHeader(&sip.FromHeader{
		Address: *aorURI,
		Params:  sip.HeaderParams{{K: "tag", V: fromTag}},
	})
	req.AppendHeader(&sip.ContactHeader{Address: contactURI})
	req.AppendHeader(&callID)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: sip.REGISTER})
	req.AppendHeader(&maxForwards)
	req.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(int(profile.Expires/time.Second))))
	req.AppendHeader(sip.NewHeader("User-Agent", UserAgent))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, NOTIFY, REFER, MESSAGE, OPTIONS, INFO, SUBSCRIBE, REGISTER"))
	if authHeaderName != "" && authHeaderValue != "" {
		req.AppendHeader(sip.NewHeader(authHeaderName, authHeaderValue))
	}
	return req
}

func parseRegistrationURI(raw string, fallbackTransport Transport) (URI, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "sip:")
	raw = strings.TrimPrefix(raw, "sips:")
	transport := fallbackTransport
	if i := strings.Index(raw, ";"); i >= 0 {
		params := raw[i+1:]
		raw = raw[:i]
		for _, p := range strings.Split(params, ";") {
			if key, val, ok := strings.Cut(p, "="); ok && strings.EqualFold(key, "transport") {
				transport = Transport(strings.ToLower(val))
			}
		}
	}
	return CreateURIFromUserAndAddress("", raw, transport), nil
}

func normalizeRegisterAuthURI(registrar URI, includePort bool, includeTransport bool) string {
	return buildSIPURI(registrar.GetHost(), registrar.Transport, includePort, includeTransport)
}

func buildSIPURI(host string, transport Transport, includePort bool, includeTransport bool) string {
	if host == "" {
		return ""
	}
	uri := "sip:" + host
	if includePort {
		port := 5060
		if transport == TransportTLS {
			port = 5061
		}
		uri += ":" + strconv.Itoa(port)
	}
	if includeTransport && transport != "" {
		uri += ";transport=" + string(transport)
	}
	return uri
}

func hostFromAddress(address string) string {
	host := address
	host = strings.TrimPrefix(host, "sip:")
	host = strings.TrimPrefix(host, "sips:")
	if i := strings.Index(host, ";"); i >= 0 {
		host = host[:i]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

func hostFromRawURI(raw string) string {
	raw = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(raw), "sip:"), "sips:")
	if i := strings.Index(raw, ";"); i >= 0 {
		raw = raw[:i]
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	return strings.ToLower(raw)
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}

func stringsForAuthHeader(authHeaderName string) string {
	switch authHeaderName {
	case "Authorization":
		return "WWW-Authenticate"
	case "Proxy-Authorization":
		return "Proxy-Authenticate"
	default:
		return ""
	}
}
