package ceos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	clablinks "github.com/srl-labs/containerlab/links"
	clabnodes "github.com/srl-labs/containerlab/nodes"
	clabtypes "github.com/srl-labs/containerlab/types"
	clabutils "github.com/srl-labs/containerlab/utils"
)

func TestDataIntfToEosName(t *testing.T) {
	tests := map[string]struct {
		ifName  string
		want    string
		wantErr bool
	}{
		"eth single":        {ifName: "eth1", want: "Ethernet1"},
		"et single":         {ifName: "et1", want: "Ethernet1"},
		"eth multi digit":   {ifName: "eth10", want: "Ethernet10"},
		"eth breakout":      {ifName: "eth1_1", want: "Ethernet1/1"},
		"eth breakout2":     {ifName: "eth2_1_1", want: "Ethernet2/1/1"},
		"et breakout":       {ifName: "et5_2", want: "Ethernet5/2"},
		"unmappable name":   {ifName: "foo", wantErr: true},
		"eth0 unmappable":   {ifName: "eth0", want: "Ethernet0"},
		"dotted unmappable": {ifName: "eth1.100", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := dataIntfToEosName(tc.ifName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.ifName, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.ifName, err)
			}
			if got != tc.want {
				t.Fatalf("dataIntfToEosName(%q) = %q, want %q", tc.ifName, got, tc.want)
			}
		})
	}
}

func newTestCeos(cfg *clabtypes.NodeConfig, eps []clablinks.Endpoint) *ceos {
	n := &ceos{DefaultNode: clabnodes.DefaultNode{Cfg: cfg}}
	n.OverwriteNode = n
	n.Endpoints = eps
	return n
}

func testEndpoint(name string) clablinks.Endpoint {
	return &clablinks.EndpointVeth{
		EndpointGeneric: clablinks.EndpointGeneric{IfaceName: name},
	}
}

func TestGenIntfMapping(t *testing.T) {
	tests := map[string]struct {
		env          map[string]string
		endpoints    []clablinks.Endpoint
		generated    bool
		wantErr      bool
		wantMgmtIntf string
		wantMapping  *eosIntfMapping
	}{
		"no env: no generation": {
			env:          nil,
			endpoints:    []clablinks.Endpoint{testEndpoint("eth1")},
			generated:    false,
			wantMgmtIntf: "",
		},
		"env set: generates full mapping": {
			env:          map[string]string{intfMapEnvVar: "Management1"},
			endpoints:    []clablinks.Endpoint{testEndpoint("eth1"), testEndpoint("eth2")},
			generated:    true,
			wantMgmtIntf: "Management1",
			wantMapping: &eosIntfMapping{
				ManagementIntf: map[string]string{"eth0": "Management1"},
				EthernetIntf:   map[string]string{"eth1": "Ethernet1", "eth2": "Ethernet2"},
			},
		},
		"env set: preserves breakout shorthand": {
			env:          map[string]string{intfMapEnvVar: "Management1"},
			endpoints:    []clablinks.Endpoint{testEndpoint("eth1_1"), testEndpoint("eth2_1_1")},
			generated:    true,
			wantMgmtIntf: "Management1",
			wantMapping: &eosIntfMapping{
				ManagementIntf: map[string]string{"eth0": "Management1"},
				EthernetIntf:   map[string]string{"eth1_1": "Ethernet1/1", "eth2_1_1": "Ethernet2/1/1"},
			},
		},
		"env set: no data interfaces": {
			env:          map[string]string{intfMapEnvVar: "Management1"},
			endpoints:    nil,
			generated:    true,
			wantMgmtIntf: "Management1",
			wantMapping: &eosIntfMapping{
				ManagementIntf: map[string]string{"eth0": "Management1"},
				EthernetIntf:   map[string]string{},
			},
		},
		"invalid mgmt name: error": {
			env:       map[string]string{intfMapEnvVar: "eth0"},
			endpoints: []clablinks.Endpoint{testEndpoint("eth1")},
			wantErr:   true,
		},
		"unmappable data interface: error": {
			env:       map[string]string{intfMapEnvVar: "Management1"},
			endpoints: []clablinks.Endpoint{testEndpoint("foo")},
			wantErr:   true,
		},
		"dotted subinterface: error": {
			env:       map[string]string{intfMapEnvVar: "Management1"},
			endpoints: []clablinks.Endpoint{testEndpoint("eth1.100")},
			wantErr:   true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			labDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(labDir, "flash"), 0o750); err != nil {
				t.Fatal(err)
			}

			cfg := &clabtypes.NodeConfig{
				ShortName: "ceos1",
				LabDir:    labDir,
				Env:       tc.env,
			}
			n := newTestCeos(cfg, tc.endpoints)

			generated, err := n.genIntfMapping(cfg)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got generated=%v", generated)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if generated != tc.generated {
				t.Fatalf("generated = %v, want %v", generated, tc.generated)
			}
			if cfg.MgmtIntf != tc.wantMgmtIntf {
				t.Fatalf("MgmtIntf = %q, want %q", cfg.MgmtIntf, tc.wantMgmtIntf)
			}

			mapPath := filepath.Join(labDir, "flash", eosIntfMappingFile)
			if !tc.generated {
				if _, statErr := os.Stat(mapPath); statErr == nil {
					t.Fatalf("expected no %s to be written", eosIntfMappingFile)
				}
				return
			}

			b, readErr := os.ReadFile(mapPath)
			if readErr != nil {
				t.Fatalf("failed to read generated mapping: %v", readErr)
			}
			var got eosIntfMapping
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("generated mapping is not valid json: %v", err)
			}
			if diff := cmp.Diff(*tc.wantMapping, got); diff != "" {
				t.Fatalf("generated mapping mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// genIntfMapping must remove a mapping file left by a previous autogen run when
// INTF_MAP_ETH0 is no longer set, so a stale map isn't applied on redeploy.
func TestGenIntfMappingRemovesStaleFile(t *testing.T) {
	labDir := t.TempDir()
	flash := filepath.Join(labDir, "flash")
	if err := os.MkdirAll(flash, 0o750); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(flash, eosIntfMappingFile)
	if err := os.WriteFile(stale, []byte(`{"ManagementIntf":{"eth0":"Management1"}}`), 0o640); err != nil {
		t.Fatal(err)
	}

	cfg := &clabtypes.NodeConfig{ShortName: "ceos1", LabDir: labDir, Env: map[string]string{}}
	n := newTestCeos(cfg, []clablinks.Endpoint{testEndpoint("eth1")})

	generated, err := n.genIntfMapping(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if generated {
		t.Fatalf("generated = true, want false")
	}
	if _, statErr := os.Stat(stale); !os.IsNotExist(statErr) {
		t.Fatalf("expected stale %s to be removed, stat err = %v", eosIntfMappingFile, statErr)
	}
}

// Init must strip INTF_MAP_ETH0 (from both the env and the baked-in Cmd) when
// the user binds their own EosIntfMapping.json, so the bound file stays
// authoritative even on images that consume the variable natively.
func TestInitStripsIntfMapEnvWithUserBind(t *testing.T) {
	cfg := &clabtypes.NodeConfig{
		ShortName: "ceos1",
		LabDir:    t.TempDir(),
		Env:       map[string]string{intfMapEnvVar: "Management1"},
		Binds:     []string{"/host/EosIntfMapping.json:/mnt/flash/EosIntfMapping.json"},
		Certificate: &clabtypes.CertificateConfig{
			Issue: clabutils.Pointer(false),
		},
	}

	n := &ceos{}
	if err := n.Init(cfg); err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	if _, present := cfg.Env[intfMapEnvVar]; present {
		t.Fatalf("expected %s to be removed from node env", intfMapEnvVar)
	}
	if strings.Contains(cfg.Cmd, intfMapEnvVar) {
		t.Fatalf("expected %s not to be baked into Cmd, got: %q", intfMapEnvVar, cfg.Cmd)
	}
}

func TestSetMgmtInterface(t *testing.T) {
	tests := map[string]struct {
		mapping  *eosIntfMapping
		wantIntf string
	}{
		"no bind: default Management0": {
			mapping:  nil,
			wantIntf: "Management0",
		},
		"bind renames eth0": {
			mapping:  &eosIntfMapping{ManagementIntf: map[string]string{"eth0": "Management1"}},
			wantIntf: "Management1",
		},
		"bind without eth0 entry keeps default": {
			mapping:  &eosIntfMapping{EthernetIntf: map[string]string{"eth1": "Ethernet1"}},
			wantIntf: "Management0",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &clabtypes.NodeConfig{ShortName: "ceos1"}
			if tc.mapping != nil {
				dir := t.TempDir()
				mapPath := filepath.Join(dir, eosIntfMappingFile)
				b, err := json.Marshal(tc.mapping)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(mapPath, b, 0o640); err != nil {
					t.Fatal(err)
				}
				cfg.Binds = []string{mapPath + ":/mnt/flash/" + eosIntfMappingFile}
			}

			if err := setMgmtInterface(cfg); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.MgmtIntf != tc.wantIntf {
				t.Fatalf("MgmtIntf = %q, want %q", cfg.MgmtIntf, tc.wantIntf)
			}
		})
	}
}

// guard against accidental leakage of the trigger var into a non-opted node
func TestGenIntfMappingNoEnvNoFile(t *testing.T) {
	labDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(labDir, "flash"), 0o750)
	cfg := &clabtypes.NodeConfig{ShortName: "ceos1", LabDir: labDir, Env: map[string]string{"OTHER": "x"}}
	n := newTestCeos(cfg, []clablinks.Endpoint{testEndpoint("eth1")})

	generated, err := n.genIntfMapping(cfg)
	if err != nil || generated {
		t.Fatalf("expected no generation, got generated=%v err=%v", generated, err)
	}
	if _, err := os.Stat(filepath.Join(labDir, "flash", eosIntfMappingFile)); !os.IsNotExist(err) {
		t.Fatalf("expected no mapping file, stat err = %v", err)
	}
}
