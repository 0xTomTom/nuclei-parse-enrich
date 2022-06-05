package enricher

/*
* https://www.DIVD.nl
* written by Pepijn van der Stap
 */

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"nuclei-parse-enrich/pkg/ripestat"
	"nuclei-parse-enrich/pkg/types"

	"github.com/likexian/whois"
	"github.com/sirupsen/logrus"
)

var (
	whoisRegexp = regexp.MustCompile("[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*")
)

const (
	ripeStatSourceApp = "AS50559-DIVD_NL"
)

type Enricher struct {
	types.EnrichInfo
	rs *ripestat.Client
}

func NewEnricher(ip string) *Enricher {
	c := ripestat.NewRipeStatClient(ripeStatSourceApp)
	return &Enricher{
		EnrichInfo: types.EnrichInfo{
			Ip: ip,
		},
		rs: c,
	}
}

func (e *Enricher) Enrich() *types.EnrichInfo {
	return e.EnrichIP(e.Ip)
}

func (e *Enricher) EnrichIP(ipAddr string) *types.EnrichInfo {
	e.EnrichInfo = types.EnrichInfo{
		Ip: ipAddr,
	}

	e.EnrichInfo.Abuse, e.EnrichInfo.Abuse_source = e.enrichAbuseFromIP(ipAddr)
	e.EnrichInfo.Prefix, e.EnrichInfo.Asn = e.enrichPrefixAndASNFromIP(ipAddr)
	e.EnrichInfo.Holder = e.enrichHolderFromASN(e.EnrichInfo.Asn)
	e.EnrichInfo.City, e.EnrichInfo.Country = e.enrichCityAndCountryFromPrefix(e.EnrichInfo.Prefix)

	return &e.EnrichInfo
}

func (e *Enricher) enrichAbuseFromIP(ipAddr string) (string, string) {
	abuse := "unknown"
	abuseSource := ""

	contacts, err := e.rs.GetAbuseContacts(ipAddr)
	if err != nil {
		logrus.Warnf("abuse contacts err: %v", err)
		return abuse, abuseSource
	}

	if len(contacts) > 0 {
		return strings.Join(contacts, ";"), "ripeSTAT"
	}

	// Fallback to whois
	contacts_from_whois := e.whoisEnrichment()
	if len(contacts_from_whois) > 0 {
		return strings.Join(contacts_from_whois, ";"), "whois"
	}

	return abuse, abuseSource
}

func (e *Enricher) enrichPrefixAndASNFromIP(ipAddr string) (string, string) {
	prefix := "unknown"
	asn := "unknown"

	netInfo, err := e.rs.GetNetworkInfo(ipAddr)
	if err != nil {
		logrus.Warnf("network info err: %v", err)
		return prefix, asn
	}

	if len(netInfo.ASNs) == 0 {
		return netInfo.Prefix, asn
	}

	return netInfo.Prefix, netInfo.ASNs[0]
}

func (e *Enricher) enrichHolderFromASN(asn string) string {
	holder := "unknown"

	if asn == "unknown" {
		return holder
	}

	asOverview, err := e.rs.GetASOverview(asn)
	if err != nil {
		logrus.Warnf("holder err: %v", err)
		return holder
	}

	return asOverview.Holder
}

func (e *Enricher) enrichCityAndCountryFromPrefix(prefix string) (string, string) {
	city := "unknown"
	country := "unknown"

	if prefix == "unknown" {
		return city, country
	}

	geolocation, err := e.rs.GetGeolocationData(prefix)
	if err != nil {
		logrus.Warnf("geolocation err: %v", err)
		return city, country
	}

	if len(geolocation.LocatedResources) == 0 {
		return city, country
	}

	if len(geolocation.LocatedResources[0].Locations) == 0 {
		return city, country
	}

	return geolocation.LocatedResources[0].Locations[0].City, geolocation.LocatedResources[0].Locations[0].Country
}

func (e *Enricher) whoisEnrichment() []string {
	return e.whoisEnrichmentIP(e.Ip)
}

func (e *Enricher) whoisEnrichmentIP(ipAddr string) []string {
	logrus.Debug("enricher: ripestat has no abuse mails for us, executing whoisEnrichment on IP address: ", ipAddr)

	whoisInfo, err := whois.Whois(ipAddr)
	if err != nil || whoisInfo == "" {
		logrus.Debug("enricher: whoisEnrichment - could not get whois info for ", ipAddr)
		return []string{}
	}

	foundMailAddresses := whoisRegexp.FindAllString(whoisInfo, -1)
	switch len(foundMailAddresses) {
	case 0:
		logrus.Debug("enricher: whoisEnrichment - could not find any abuse emails for ", ipAddr)
		return []string{}
	case 1:
		// Spare some allocations and a sort if there's only one address found
		return []string{strings.ToLower(foundMailAddresses[0])}
	}

	// lower and sort unique
	m := make(map[string]struct{}, len(foundMailAddresses))
	for _, v := range foundMailAddresses {
		m[strings.ToLower(v)] = struct{}{}
	}

	abusemails := make([]string, 0, len(m))
	for k := range m {
		abusemails = append(abusemails, k)
	}
	sort.Strings(abusemails)

	return abusemails
}

func (e *Enricher) queryRipeStat(resource string, query string) (map[string]interface{}, error) {
	if query == "" {
		return nil, fmt.Errorf("empty query for resource %v", resource)
	}
	url := fmt.Sprintf("https://stat.ripe.net/data/%s/data.json?resource=%s&sourceapp=%s", resource, query, ripeStatSourceApp)

	resp, err := http.Get(url)
	if err != nil {
		logrus.Debug("enricher: queryRipeStat - could not get data from ", url)
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logrus.Debug("enricher: queryRipeStat - could not read response body from ", url)
		return nil, err
	}

	var data map[string]interface{}

	err = json.Unmarshal(body, &data)
	if err != nil {
		logrus.Debug("enricher: queryRipeStat - could not unmarshal response body from ", url)
		return nil, err
	}
	if data == nil {
		return data, errors.New("enricher: ripestat is down " + url)
	}
	return data, nil
}
