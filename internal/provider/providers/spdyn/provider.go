package spdyn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/qdm12/ddns-updater/internal/models"
	"github.com/qdm12/ddns-updater/internal/provider/constants"
	"github.com/qdm12/ddns-updater/internal/provider/errors"
	"github.com/qdm12/ddns-updater/internal/provider/headers"
	"github.com/qdm12/ddns-updater/internal/provider/utils"
	"github.com/qdm12/ddns-updater/pkg/publicip/ipversion"
)

type Provider struct {
	domain     string
	owner      string
	ipVersion  ipversion.IPVersion
	ipv6Suffix netip.Prefix
	user       string
	password   string
	token      string
}

func New(data json.RawMessage, domain, owner string,
	ipVersion ipversion.IPVersion, ipv6Suffix netip.Prefix) (
	p *Provider, err error,
) {
	extraSettings := struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}{}
	err = json.Unmarshal(data, &extraSettings)
	if err != nil {
		return nil, err
	}

	err = validateSettings(domain, owner, extraSettings.Token, extraSettings.User, extraSettings.Password)
	if err != nil {
		return nil, fmt.Errorf("validating provider specific settings: %w", err)
	}

	return &Provider{
		domain:     domain,
		owner:      owner,
		ipVersion:  ipVersion,
		ipv6Suffix: ipv6Suffix,
		user:       extraSettings.User,
		password:   extraSettings.Password,
		token:      extraSettings.Token,
	}, nil
}

func validateSettings(domain, owner, token, user, password string) (err error) {
	err = utils.CheckDomain(domain)
	if err != nil {
		return fmt.Errorf("%w: %w", errors.ErrDomainNotValid, err)
	}

	if owner == "*" {
		return fmt.Errorf("%w", errors.ErrOwnerWildcard)
	}

	if token == "" {
		switch {
		case user == "":
			return fmt.Errorf("%w", errors.ErrUsernameNotSet)
		case password == "":
			return fmt.Errorf("%w", errors.ErrPasswordNotSet)
		}
	}

	return nil
}

func (p *Provider) String() string {
	return utils.ToString(p.domain, p.owner, constants.Spdyn, p.ipVersion)
}

func (p *Provider) Domain() string {
	return p.domain
}

func (p *Provider) Owner() string {
	return p.owner
}

func (p *Provider) IPVersion() ipversion.IPVersion {
	return p.ipVersion
}

func (p *Provider) IPv6Suffix() netip.Prefix {
	return p.ipv6Suffix
}

func (p *Provider) Proxied() bool {
	return false
}

func (p *Provider) BuildDomainName() string {
	return utils.BuildDomainName(p.owner, p.domain)
}

func (p *Provider) HTML() models.HTMLRow {
	return models.HTMLRow{
		Domain:    fmt.Sprintf("<a href=\"http://%s\">%s</a>", p.BuildDomainName(), p.BuildDomainName()),
		Owner:     p.Owner(),
		Provider:  "<a href=\"https://spdyn.com/\">Spdyn DNS</a>",
		IPVersion: p.ipVersion.String(),
	}
}

func (p *Provider) Update(ctx context.Context, client *http.Client, ip netip.Addr) (newIP netip.Addr, err error) {
	// see https://wiki.securepoint.de/SPDyn/Variablen
	u := url.URL{
		Scheme: "https",
		Host:   "update.spdyn.de",
		Path:   "/nic/update",
	}
	hostname := utils.BuildURLQueryHostname(p.owner, p.domain)
	values := url.Values{}
	values.Set("hostname", hostname)
	values.Set("myip", ip.String())
	if p.token != "" {
		values.Set("user", hostname)
		values.Set("pass", p.token)
	} else {
		values.Set("user", p.user)
		values.Set("pass", p.password)
	}
	u.RawQuery = values.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("creating http request: %w", err)
	}
	headers.SetUserAgent(request)

	response, err := client.Do(request)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("doing http request: %w", err)
	}
	defer response.Body.Close()

	bodyString, err := utils.ReadAndCleanBody(response.Body)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("reading response: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("%w: %d: %s",
			errors.ErrHTTPStatusNotValid, response.StatusCode, utils.ToSingleLine(bodyString))
	}

	switch {
	case isAny(bodyString, constants.Abuse, "numhost"):
		return netip.Addr{}, fmt.Errorf("%w", errors.ErrBannedAbuse)
	case isAny(bodyString, constants.Badauth, "!yours"):
		return netip.Addr{}, fmt.Errorf("%w", errors.ErrAuth)
	case strings.HasPrefix(bodyString, "good"):
		return ip, nil
	case bodyString == constants.Notfqdn:
		return netip.Addr{}, fmt.Errorf("%w: not fqdn", errors.ErrBadRequest)
	case strings.HasPrefix(bodyString, "nochg"):
		return ip, nil
	case isAny(bodyString, "nohost", "fatal"):
		return netip.Addr{}, fmt.Errorf("%w", errors.ErrHostnameNotExists)
	default:
		return netip.Addr{}, fmt.Errorf("%w: %s", errors.ErrUnknownResponse, bodyString)
	}
}

func isAny(s string, values ...string) (ok bool) {
	for _, value := range values {
		if s == value {
			return true
		}
	}
	return false
}
