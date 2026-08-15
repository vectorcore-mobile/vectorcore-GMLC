package delivery

import "github.com/vectorcore/gmlc/internal/domain"

var qosClassNames = map[uint32]string{
	domain.QoSClassAssured:    "assured",
	domain.QoSClassBestEffort: "best_effort",
}
var responseTimeNames = map[uint32]string{
	domain.ResponseTimeLowDelay:      "low_delay",
	domain.ResponseTimeDelayTolerant: "delay_tolerant",
}
var locationTypeNames = map[uint32]string{
	domain.LocationCurrent:            "current",
	domain.LocationCurrentOrLastKnown: "current_or_last_known",
}

// qosFromDomain renders a stored request's QoS back into the JSON shape
// RequestJSON uses. Returns nil if q is nil, matching how an omitted "qos"
// object round-trips to no AVP and back to no object.
func qosFromDomain(q *domain.QoSRequest) map[string]any {
	if q == nil {
		return nil
	}
	out := map[string]any{}
	if q.Class != nil {
		out["class"] = qosClassNames[*q.Class]
	}
	if q.HorizontalAccuracyMeters != nil {
		out["horizontal_accuracy_meters"] = *q.HorizontalAccuracyMeters
	}
	if q.VerticalAccuracyMeters != nil {
		out["vertical_accuracy_meters"] = *q.VerticalAccuracyMeters
	}
	if q.VerticalRequested {
		out["vertical_requested"] = true
	}
	if q.ResponseTimeClass != nil {
		out["response_time"] = responseTimeNames[*q.ResponseTimeClass]
	}
	return out
}

// RequestJSON builds the JSON representation of a single request/result
// pair, used as the async-completion webhook payload (see
// cmd/gmlc/main.go's orchestrator completion hook).
func RequestJSON(r domain.Request, v domain.Result) map[string]any {
	out := map[string]any{"id": r.ID, "service_type": r.Service, "state": r.State, "failure_code": r.FailureCode, "location_type": locationTypeNames[r.LocationType], "created_at": r.CreatedAt, "updated_at": r.UpdatedAt}
	if r.Priority != nil {
		out["priority"] = *r.Priority
	}
	if qos := qosFromDomain(r.QoS); qos != nil {
		out["qos"] = qos
	}
	// A completed request always has a result row, but not every completion
	// has a position — additional_information (ECGI-only) and Polygon
	// (no single center point) completions don't. Gating on Latitude/
	// Longitude here would silently drop those results from the payload
	// even though the request genuinely completed.
	if r.State == domain.StateCompleted {
		result := map[string]any{"created_at": v.CreatedAt}
		if v.Shape != "" {
			result["shape"] = v.Shape
		}
		if v.Latitude != nil && v.Longitude != nil {
			result["latitude"] = *v.Latitude
			result["longitude"] = *v.Longitude
		}
		if len(v.ECGI) > 0 {
			result["ecgi"] = v.ECGI
		}
		if v.UncertaintyMeters != nil {
			result["uncertainty_meters"] = *v.UncertaintyMeters
		}
		if v.SemiMajorMeters != nil {
			result["semi_major_meters"] = *v.SemiMajorMeters
		}
		if v.SemiMinorMeters != nil {
			result["semi_minor_meters"] = *v.SemiMinorMeters
		}
		if v.OrientationDegrees != nil {
			result["orientation_degrees"] = *v.OrientationDegrees
		}
		if v.ConfidencePercent != nil {
			result["confidence_percent"] = *v.ConfidencePercent
		}
		if v.AgeOfLocationEstimate != nil {
			result["age_of_location_estimate_minutes"] = *v.AgeOfLocationEstimate
		}
		if v.AccuracyFulfilment != nil {
			result["accuracy_fulfilment"] = *v.AccuracyFulfilment
		}
		out["result"] = result
	}
	return out
}
