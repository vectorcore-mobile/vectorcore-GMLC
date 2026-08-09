package slg

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/fiorix/go-diameter/v4/diam/dict"
)

var dictionaryOnce sync.Once
var dictionaryErr error

// EnsureDictionary registers the SLg commands with the shared Diameter
// dictionary. Without it, dict.Default has no entry for application
// 16777255, so ServeMux.ServeDIAM's FindCommand lookup fails on an incoming
// PLA and falls through to the catch-all index instead of the exact-match
// handler registered in registry.go, and the answer is silently dropped.
func EnsureDictionary() error {
	dictionaryOnce.Do(func() { dictionaryErr = dict.Default.Load(bytes.NewBufferString(dictionaryXML)) })
	return dictionaryErr
}

func init() {
	if err := EnsureDictionary(); err != nil {
		panic(fmt.Sprintf("slg: load Diameter dictionary: %v", err))
	}
}

const dictionaryXML = `<?xml version="1.0" encoding="UTF-8"?>
<diameter><application id="16777255" type="auth" name="3GPP SLg">
<vendor id="10415" name="3GPP"/>
<command code="8388620" short="PL" name="Provide-Location"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command>
<command code="8388621" short="LR" name="Location-Report"><request><rule avp="AVP" required="false"/></request><answer><rule avp="AVP" required="false"/></answer></command>
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
<avp name="Vendor-Id" code="266" vendor-id="0" must="M" may-encrypt="N"><data type="Unsigned32"/></avp>
<avp name="Failed-AVP" code="279" vendor-id="0" must="M" may-encrypt="N"><data type="Grouped"><rule avp="AVP" required="false"/></data></avp>
<avp name="LCS-Client-Type" code="1241" vendor-id="0" must="M" may-encrypt="N"><data type="Enumerated"/></avp>
<avp name="MSISDN" code="701" vendor-id="10415" must="M,V" may-encrypt="N"><data type="OctetString"/></avp>
<avp name="IMEI" code="1402" vendor-id="10415" must="M,V" may-encrypt="N"><data type="UTF8String"/></avp>
<avp name="SLg-Location-Type" code="2500" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Enumerated"/></avp>
<avp name="LCS-EPS-Client-Name" code="2501" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Grouped"><rule avp="AVP" required="false"/></data></avp>
<avp name="Location-Event" code="2518" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Enumerated"/></avp>
<avp name="ECGI" code="2517" vendor-id="10415" must="M,V" may-encrypt="N"><data type="OctetString"/></avp>
<avp name="LCS-Reference-Number" code="2531" vendor-id="10415" must="M,V" may-encrypt="N"><data type="OctetString"/></avp>
<avp name="Location-Estimate" code="1242" vendor-id="0" must="M" may-encrypt="N"><data type="OctetString"/></avp>
<avp name="Accuracy-Fulfilment-Indicator" code="2513" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Enumerated"/></avp>
<avp name="Age-Of-Location-Estimate" code="2514" vendor-id="10415" must="M,V" may-encrypt="N"><data type="Unsigned32"/></avp>
<avp name="Velocity-Estimate" code="2515" vendor-id="10415" must="M,V" may-encrypt="N"><data type="OctetString"/></avp>
<avp name="EUTRAN-Positioning-Data" code="2516" vendor-id="10415" must="M,V" may-encrypt="N"><data type="OctetString"/></avp>
</application></diameter>`
