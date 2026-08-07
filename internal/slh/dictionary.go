package slh

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/fiorix/go-diameter/v4/diam/dict"
)

var dictionaryOnce sync.Once
var dictionaryErr error

// EnsureDictionary registers the SLh command with the shared Diameter
// dictionary. Without it, dict.Default has no entry for application
// 16777291, so ServeMux.ServeDIAM's FindCommand lookup fails on an incoming
// RIA and falls through to the catch-all index — which is never a delivery
// path here — instead of the exact-match handler registered in registry.go.
// The result is a silently dropped answer rather than a routed one.
func EnsureDictionary() error {
	dictionaryOnce.Do(func() { dictionaryErr = dict.Default.Load(bytes.NewBufferString(dictionaryXML)) })
	return dictionaryErr
}

func init() {
	if err := EnsureDictionary(); err != nil {
		panic(fmt.Sprintf("slh: load Diameter dictionary: %v", err))
	}
}

const dictionaryXML = `<?xml version="1.0" encoding="UTF-8"?>
<diameter><application id="16777291" type="auth" name="3GPP SLh">
<vendor id="10415" name="3GPP"/>
<command code="8388622" short="RI" name="Routing-Info"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command>
<avp name="Session-Id" code="263" vendor-id="0" must="M" may-encrypt="N"><data type="UTF8String"/></avp>
<avp name="Auth-Session-State" code="277" vendor-id="0" must="M" may-encrypt="N"><data type="Enumerated"/></avp>
<avp name="Origin-Host" code="264" vendor-id="0" must="M" may-encrypt="N"><data type="DiameterIdentity"/></avp>
<avp name="Origin-Realm" code="296" vendor-id="0" must="M" may-encrypt="N"><data type="DiameterIdentity"/></avp>
<avp name="Destination-Host" code="293" vendor-id="0" must="M" may-encrypt="N"><data type="DiameterIdentity"/></avp>
<avp name="Destination-Realm" code="283" vendor-id="0" must="M" may-encrypt="N"><data type="DiameterIdentity"/></avp>
<avp name="User-Name" code="1" vendor-id="0" must="M" may-encrypt="N"><data type="UTF8String"/></avp>
<avp name="Result-Code" code="268" vendor-id="0" must="M" may-encrypt="N"><data type="Unsigned32"/></avp>
<avp name="Experimental-Result" code="297" vendor-id="0" must="M" may-encrypt="N"><data type="Grouped"><rule avp="AVP" required="false"/></data></avp>
<avp name="Experimental-Result-Code" code="298" vendor-id="0" must="M" may-encrypt="N"><data type="Unsigned32"/></avp>
<avp name="MSISDN" code="701" vendor-id="10415" must="M,V" may-encrypt="N"><data type="OctetString"/></avp>
<avp name="Serving-Node" code="2401" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Grouped"><rule avp="AVP" required="false"/></data></avp>
<avp name="MME-Name" code="2402" vendor-id="10415" must="M,V" may-encrypt="N"><data type="DiameterIdentity"/></avp>
<avp name="MME-Realm" code="2408" vendor-id="10415" must="M,V" may-encrypt="N"><data type="DiameterIdentity"/></avp>
</application></diameter>`
