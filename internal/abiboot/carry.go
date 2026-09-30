package abiboot

import (
	"bytes"
	"encoding/json"

	"github.com/collegeming/cpa-jethub-plugins/internal/jethub/credjson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// carryFileMembers folds the members of the auth file the host is holding into
// every credential payload a handler returns.
//
// The host replaces the credential it stores with whatever a plugin reports, and
// it writes that payload back to the auth file. A response that serialises only
// the plugin's own struct therefore erases the members the host owns: `priority`
// and `weight` live in that same file and drive credential selection, so losing
// them silently resets a credential's routing tier — and it happens on the parse
// that follows any file change, not only on the refresh that triggered it.
//
// Merging here rather than in each provider keeps the rule in one place: a
// handler keeps returning its own credential, and the file's other members ride
// along untouched.
func carryFileMembers(host *Host, result any) any {
	if host == nil || result == nil || len(bytes.TrimSpace(host.Incoming)) == 0 {
		return result
	}
	incoming := host.Incoming
	switch response := result.(type) {
	case pluginapi.AuthParseResponse:
		response.Auth = withFileMembers(incoming, response.Auth)
		response.Auths = withFileMemberList(incoming, response.Auths)
		return response
	case *pluginapi.AuthParseResponse:
		if response == nil {
			return result
		}
		response.Auth = withFileMembers(incoming, response.Auth)
		response.Auths = withFileMemberList(incoming, response.Auths)
		return response
	case pluginapi.AuthLoginPollResponse:
		response.Auth = withFileMembers(incoming, response.Auth)
		response.Auths = withFileMemberList(incoming, response.Auths)
		return response
	case *pluginapi.AuthLoginPollResponse:
		if response == nil {
			return result
		}
		response.Auth = withFileMembers(incoming, response.Auth)
		response.Auths = withFileMemberList(incoming, response.Auths)
		return response
	case pluginapi.AuthRefreshResponse:
		response.Auth = withFileMembers(incoming, response.Auth)
		return response
	case *pluginapi.AuthRefreshResponse:
		if response == nil {
			return result
		}
		response.Auth = withFileMembers(incoming, response.Auth)
		return response
	}
	return result
}

func withFileMemberList(incoming json.RawMessage, auths []pluginapi.AuthData) []pluginapi.AuthData {
	for index := range auths {
		auths[index] = withFileMembers(incoming, auths[index])
	}
	return auths
}

func withFileMembers(incoming json.RawMessage, data pluginapi.AuthData) pluginapi.AuthData {
	if len(bytes.TrimSpace(data.StorageJSON)) == 0 {
		// A record without a stored payload has nothing to merge into: the host
		// keeps its own storage for that credential.
		return data
	}
	data.StorageJSON = credjson.MergePreserved(incoming, data.StorageJSON)
	return data
}
