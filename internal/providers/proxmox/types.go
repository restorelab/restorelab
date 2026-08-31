package proxmox

import (
	"fmt"
	"strconv"
)

// PVE's JSON API is notoriously loose about types: booleans travel as the
// numbers 0/1, and some fields flip between a JSON number and a JSON string
// depending on endpoint and PVE version. Rather than fight that with brittle
// structs, most responses are decoded into map[string]any (or slices of it)
// and read through these helpers, which never panic on an unexpected shape.

// asString normalises an arbitrary decoded JSON value to a string.
func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// asFloat normalises an arbitrary decoded JSON value to a float64.
func asFloat(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// asInt normalises an arbitrary decoded JSON value to an int.
func asInt(v any) int { return int(asFloat(v)) }

// asInt64 normalises an arbitrary decoded JSON value to an int64.
func asInt64(v any) int64 { return int64(asFloat(v)) }

// asBool normalises an arbitrary decoded JSON value to a bool. PVE reports
// booleans as the numbers 0/1 far more often than as JSON true/false.
func asBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		switch t {
		case "1", "true", "yes":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// idString formats a decoded vmid (always a whole number, but delivered as
// float64 by encoding/json) back into the plain decimal string RestoreLab
// uses as its provider-scoped workload ID.
func idString(v any) string { return strconv.Itoa(asInt(v)) }

// agentInterfaces is the stable-shaped response of
// /nodes/{node}/qemu/{vmid}/agent/network-get-interfaces. Unlike the
// resource/status endpoints its keys don't change type across PVE versions,
// so a plain struct is fine here.
type agentInterfaces struct {
	Result []agentInterface `json:"result"`
}

type agentInterface struct {
	Name        string           `json:"name"`
	IPAddresses []agentIPAddress `json:"ip-addresses"`
}

type agentIPAddress struct {
	IPAddress     string `json:"ip-address"`
	IPAddressType string `json:"ip-address-type"`
}
