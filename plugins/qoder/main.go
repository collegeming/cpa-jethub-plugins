// Command qoder builds the CPA c-shared plugin library for Qoder.
//
// Both files in this directory are package main: main.go carries the exported
// cgo ABI and plugin.go carries the abiboot.Plugin implementation. Keeping the
// plugin implementation in the same package is what lets
// `go build -buildmode=c-shared ./plugins/qoder` (see scripts/build.sh) emit the
// shared object directly from this directory.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	C.store_host_api(host)
	abiboot.SetHostCaller(func(method string, request []byte) ([]byte, error) {
		cMethod := C.CString(method)
		defer C.free(unsafe.Pointer(cMethod))
		var response C.cliproxy_buffer
		var requestPtr *C.uint8_t
		var payload []byte
		if len(request) > 0 {
			payload = request
			cPayload := C.CBytes(payload)
			if cPayload == nil {
				return nil, abiboot.Errorf("host_alloc", "allocate host request")
			}
			defer C.free(cPayload)
			requestPtr = (*C.uint8_t)(cPayload)
		}
		C.call_host_api(cMethod, requestPtr, C.size_t(len(payload)), &response)
		var raw []byte
		if response.ptr != nil && response.len > 0 {
			raw = C.GoBytes(response.ptr, C.int(response.len))
		}
		if response.ptr != nil {
			C.free_host_buffer(response.ptr, response.len)
		}
		return raw, nil
	})
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if method == nil {
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw := abiboot.Dispatch(Plugin(), C.GoString(method), payload)
	if response == nil || len(raw) == 0 {
		return 0
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return 1
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	abiboot.ClearHostCaller()
}

var _ = json.Marshal
