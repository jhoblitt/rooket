// Package zot holds what rooket's two zot containers — every cluster's local
// registry and the host-wide pull-through cache — must agree on: the pinned
// image and the subset of zot's configuration schema rooket renders.
package zot

const (
	// Image is pinned so both container roles run the same deliberate zot
	// version: an unpinned tag would recreate them — resyncing the cache and
	// discarding a registry's pushed images — on every silent upstream
	// rebuild.
	//
	// It is the multi-architecture index rather than one of zot's per-arch
	// repositories, because rooket itself is released for linux/arm64: an
	// amd64-only pin leaves the registry, which no cluster can build or push
	// without, unable to run at all on those hosts.
	Image = "ghcr.io/project-zot/zot:v2.1.17"

	// DistSpecVersion is the OCI distribution-spec version every generated
	// config declares.
	DistSpecVersion = "1.1.1"

	// InternalPort is the port zot listens on inside its container.
	InternalPort = 5000

	// ConfigPath is where a generated config is bind-mounted in the
	// container.
	ConfigPath = "/etc/zot/config.json"

	// StoragePath is zot's root directory inside the container.
	StoragePath = "/var/lib/zot"
)

type Storage struct {
	RootDirectory string `json:"rootDirectory"`
	GC            bool   `json:"gc"`
}

type HTTP struct {
	Address string   `json:"address"`
	Port    string   `json:"port"`
	Compat  []string `json:"compat,omitempty"`
}

type Log struct {
	Level string `json:"level"`
}

type Content struct {
	Prefix      string `json:"prefix"`
	Destination string `json:"destination"`
}

type SyncRegistry struct {
	URLs      []string  `json:"urls"`
	Content   []Content `json:"content"`
	OnDemand  bool      `json:"onDemand"`
	TLSVerify bool      `json:"tlsVerify"`
}

type Sync struct {
	Enable     bool           `json:"enable"`
	Registries []SyncRegistry `json:"registries"`
}

type Extensions struct {
	Sync Sync `json:"sync"`
}

// Config is the top-level document 'zot serve' reads. Extensions is a
// pointer so a config without any — the per-cluster registry's — omits the
// key entirely rather than declaring a disabled sync section.
type Config struct {
	DistSpecVersion string      `json:"distSpecVersion"`
	Storage         Storage     `json:"storage"`
	HTTP            HTTP        `json:"http"`
	Log             Log         `json:"log"`
	Extensions      *Extensions `json:"extensions,omitempty"`
}
