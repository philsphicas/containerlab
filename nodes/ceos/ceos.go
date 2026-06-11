// Copyright 2020 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package ceos

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/charmbracelet/log"
	clabconstants "github.com/srl-labs/containerlab/constants"
	clabexec "github.com/srl-labs/containerlab/exec"
	clabnodes "github.com/srl-labs/containerlab/nodes"
	clabtypes "github.com/srl-labs/containerlab/types"
	clabutils "github.com/srl-labs/containerlab/utils"
)

const (
	ifWaitScriptContainerPath = "/mnt/flash/if-wait.sh"
	generateable              = true
	generateIfFormat          = "eth%d"

	scrapliPlatformName = "arista_eos"
	NapalmPlatformName  = "eos"

	tlsKeyFile  = "node.key"
	tlsCertFile = "node.crt"
	tlsCAFile   = "ca.crt"

	// eosIntfMappingFile is the file cEOS reads to remap kernel netdevs to EOS
	// interface names. containerlab writes it into the node's flash directory
	// (mounted at /mnt/flash) when interface-map autogeneration is requested.
	eosIntfMappingFile = "EosIntfMapping.json"

	// intfMapEnvVar, when set on a ceos node (e.g. INTF_MAP_ETH0=Management1),
	// opts the node into containerlab generating an EosIntfMapping.json that
	// renames the management interface (eth0) to the given EOS interface name
	// while preserving data interface names. It mirrors the cEOS image
	// environment variable of the same name, so topologies remain valid once the
	// image supports it natively.
	intfMapEnvVar = "INTF_MAP_ETH0"
)

var (
	KindNames = []string{"ceos", "arista_ceos"}
	// defined env vars for the ceos.
	ceosEnv = map[string]string{
		"CEOS":                                "1",
		"EOS_PLATFORM":                        "ceoslab",
		"container":                           "docker",
		"ETBA":                                "1",
		"SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT": "1",
		"INTFTYPE":                            "eth",
		"MAPETH0":                             "1",
		"MGMT_INTF":                           "eth0",
	}

	//go:embed ceos.cfg
	cfgTemplate string

	saveCmd = "Cli -p 15 -c wr"

	defaultCredentials = clabnodes.NewCredentials("admin", "admin")

	// mgmtIntfNameRegexp matches a valid EOS management interface name
	// (e.g. Management1) accepted as the INTF_MAP_ETH0 value.
	mgmtIntfNameRegexp = regexp.MustCompile(`^Management[0-9]+$`)

	// dataIntfNameRegexp parses a containerlab cEOS data interface name into its
	// numeric label and optional breakout sub-labels: ethX, etX, or the
	// ethX_Y[_Z] shorthand (capture groups: X, Y, Z).
	dataIntfNameRegexp = regexp.MustCompile(`^(?:eth|et)([0-9]+)(?:_([0-9]+))?(?:_([0-9]+))?$`)
)

// Register registers the node in the NodeRegistry.
func Register(r *clabnodes.NodeRegistry) {
	generateNodeAttributes := clabnodes.NewGenerateNodeAttributes(generateable, generateIfFormat)
	platformAttrs := &clabnodes.PlatformAttrs{
		ScrapliPlatformName: scrapliPlatformName,
		NapalmPlatformName:  NapalmPlatformName,
	}

	nrea := clabnodes.NewNodeRegistryEntryAttributes(
		defaultCredentials,
		generateNodeAttributes,
		platformAttrs,
	)

	r.Register(KindNames, func() clabnodes.Node {
		return new(ceos)
	}, nrea)
}

type ceos struct {
	clabnodes.DefaultNode
}

// eosIntfMapping represents the cEOS interface mapping file
// (/mnt/flash/EosIntfMapping.json). Each section maps a kernel netdev name to
// the EOS interface name cEOS should expose it as.
type eosIntfMapping struct {
	ManagementIntf map[string]string `json:"ManagementIntf"`
	EthernetIntf   map[string]string `json:"EthernetIntf"`
}

func (n *ceos) Init(cfg *clabtypes.NodeConfig, opts ...clabnodes.NodeOption) error {
	// Init DefaultNode
	n.DefaultNode = *clabnodes.NewDefaultNode(n)

	n.Cfg = cfg

	n.StopSignal = clabtypes.SIGRTMIN3

	for _, o := range opts {
		o(n)
	}

	n.Cfg.Env = clabutils.MergeStringMaps(ceosEnv, n.Cfg.Env)

	// If the user binds their own EosIntfMapping.json, INTF_MAP_ETH0 must not
	// reach the container, so the bound file stays authoritative even on a
	// future cEOS image that consumes the variable natively. Strip it here,
	// before the environment is baked into n.Cfg.Cmd below.
	if _, ok := n.Cfg.Env[intfMapEnvVar]; ok && userProvidedIntfMapping(n.Cfg) {
		log.Warnf(
			"node %q sets %s but also binds its own %s; the bound file is used and %s is ignored",
			n.Cfg.ShortName, intfMapEnvVar, eosIntfMappingFile, intfMapEnvVar,
		)
		delete(n.Cfg.Env, intfMapEnvVar)
	}

	// the node.Cmd should be aligned with the environment.
	// prepending original Cmd with if-wait.sh script to make sure that interfaces are available
	// before init process starts
	var envSb strings.Builder
	envSb.WriteString("bash -c '" + ifWaitScriptContainerPath + " ; exec /sbin/init ")
	for k, v := range n.Cfg.Env {
		envSb.WriteString("systemd.setenv=\"" + k + "=" + v + "\" ")
	}
	envSb.WriteString("'")

	n.Cfg.Cmd = envSb.String()
	hwa, err := clabutils.GenMac("00:1c:73")
	if err != nil {
		return err
	}
	n.Cfg.MacAddress = hwa.String()

	// create TLS certificates for the node by default.
	// The cert, key and CA files are mounted into the container
	// and can be validated with `show management security ssl certificate`.
	n.Cfg.Certificate.Issue = clabutils.Pointer(true)

	// mount config dir
	cfgPath := filepath.Join(n.Cfg.LabDir, "flash")
	n.Cfg.Binds = append(n.Cfg.Binds, fmt.Sprintf("%s:/mnt/flash/", cfgPath))

	if *n.Cfg.Certificate.Issue {
		keyPath := filepath.Join(n.Cfg.LabDir, "ssl", tlsKeyFile)
		certPath := filepath.Join(n.Cfg.LabDir, "ssl", tlsCertFile)
		caPath := filepath.Join(n.Cfg.LabDir, "ssl", tlsCAFile)

		n.Cfg.Binds = append(n.Cfg.Binds,
			fmt.Sprintf("%s:/persist/secure/ssl/keys/%s", keyPath, tlsKeyFile),
			fmt.Sprintf("%s:/persist/secure/ssl/certs/%s", certPath, tlsCertFile),
			fmt.Sprintf("%s:/persist/secure/ssl/certs/%s", caPath, tlsCAFile),
		)
	}

	return nil
}

func (n *ceos) PreDeploy(ctx context.Context, params *clabnodes.PreDeployParams) error {
	clabutils.CreateDirectory(n.Cfg.LabDir, clabconstants.PermissionsOpen)
	if *n.Cfg.Certificate.Issue {
		certificate, err := n.LoadOrGenerateCertificate(params.Cert, params.TopologyName)
		if err != nil {
			return err
		}

		caCertificate, err := params.Cert.LoadCaCert()
		if err != nil {
			return err
		}

		n.Config().TLSCert = string(certificate.Cert)
		n.Config().TLSKey = string(certificate.Key)
		n.Config().TLSAnchor = string(caCertificate.Cert)
	}
	return n.createCEOSFiles(ctx)
}

func (n *ceos) PostDeploy(ctx context.Context, _ *clabnodes.PostDeployParams) error {
	log.Infof("Running postdeploy actions for Arista cEOS '%s' node", n.Cfg.ShortName)
	return n.ceosPostDeploy(ctx)
}

func (n *ceos) SaveConfig(ctx context.Context) (*clabnodes.SaveConfigResult, error) {
	cmd, _ := clabexec.NewExecCmdFromString(saveCmd)
	execResult, err := n.RunExec(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to execute cmd: %v", n.Cfg.ShortName, err)
	}

	if execResult.GetStdErrString() != "" {
		return nil, fmt.Errorf("%s errors: %s", n.Cfg.ShortName, execResult.GetStdErrString())
	}

	cfgPath := filepath.Join(n.Cfg.LabDir, "flash", "startup-config")
	log.Infof("saved cEOS configuration from %s node to %s\n", n.Cfg.ShortName, cfgPath)

	return &clabnodes.SaveConfigResult{
		ConfigPath: cfgPath,
	}, nil
}

func (n *ceos) createCEOSFiles(ctx context.Context) error {
	nodeCfg := n.Config()
	// generate config directory
	clabutils.CreateDirectory(path.Join(n.Cfg.LabDir, "flash"),
		clabconstants.PermissionsOpen)
	cfg := filepath.Join(n.Cfg.LabDir, "flash", "startup-config")
	nodeCfg.ResStartupConfig = cfg

	// set mgmt ipv4 gateway as it is already known by now
	// since the container network has been created before we launch nodes
	// and mgmt gateway can be used in ceos.Cfg template to configure default route for mgmt
	nodeCfg.MgmtIPv4Gateway = n.Runtime.Mgmt().IPv4Gw
	nodeCfg.MgmtIPv6Gateway = n.Runtime.Mgmt().IPv6Gw

	// when the node opts into interface-map autogeneration (INTF_MAP_ETH0), write
	// the generated EosIntfMapping.json into the flash dir and derive the mgmt
	// interface name from it. Otherwise fall back to a user-provided map bind or
	// the Management0 default.
	generated, err := n.genIntfMapping(nodeCfg)
	if err != nil {
		return err
	}
	if !generated {
		// set the mgmt interface name for the node
		if err := setMgmtInterface(nodeCfg); err != nil {
			return err
		}
	}

	// use startup config file provided by a user
	// make copy of template to prevent provided startup config from mutating shared package
	// template value
	currentCfgTemplate := cfgTemplate
	if nodeCfg.StartupConfig != "" {
		c, err := os.ReadFile(nodeCfg.StartupConfig)
		if err != nil {
			return err
		}
		currentCfgTemplate = string(c)
	}

	err = n.GenerateConfig(nodeCfg.ResStartupConfig, currentCfgTemplate)
	if err != nil {
		return err
	}

	// if extras have been provided copy these into the flash directory
	if nodeCfg.Extras != nil && len(nodeCfg.Extras.CeosCopyToFlash) != 0 {
		extras := nodeCfg.Extras.CeosCopyToFlash
		flash := filepath.Join(nodeCfg.LabDir, "flash")

		for _, extrapath := range extras {
			basename := filepath.Base(extrapath)
			dest := filepath.Join(flash, basename)

			topoDir := filepath.Dir(
				filepath.Dir(nodeCfg.LabDir),
			) // topo dir is needed to resolve extrapaths
			if err := clabutils.CopyFile(ctx,
				clabutils.ResolvePath(extrapath, topoDir), dest,
				clabconstants.PermissionsFileDefault); err != nil {
				return fmt.Errorf("extras: copy-to-flash %s -> %s failed %v", extrapath, dest, err)
			}
		}
	}

	// sysmac is a system mac that is +1 to Ma0 mac
	m, err := net.ParseMAC(nodeCfg.MacAddress)
	if err != nil {
		return err
	}
	m[5]++

	sysMacPath := path.Join(nodeCfg.LabDir, "flash", "system_mac_address")

	if !clabutils.FileExists(sysMacPath) {
		err = clabutils.CreateFile(sysMacPath, m.String())
	}

	if err != nil {
		return err
	}

	// adding if-wait.sh script to flash dir
	ifScriptP := path.Join(nodeCfg.LabDir, "flash", "if-wait.sh")
	if err := clabutils.CreateFile(ifScriptP, clabutils.IfWaitScript); err != nil {
		return fmt.Errorf("failed to write if-wait.sh: %w", err)
	}
	os.Chmod(ifScriptP, clabconstants.PermissionsOpen) // skipcq: GSC-G302

	if *n.Cfg.Certificate.Issue {
		err = n.createCEOSCertificates()
	}

	return err
}

// Func that Places the Certificates in the right place and format.
func (n *ceos) createCEOSCertificates() error {
	if *n.Cfg.Certificate.Issue {
		clabutils.CreateDirectory(path.Join(n.Cfg.LabDir, "ssl"), clabconstants.PermissionsOpen)

		keyPath := filepath.Join(n.Cfg.LabDir, "ssl", tlsKeyFile)
		if err := clabutils.CreateFile(keyPath, n.Config().TLSKey); err != nil {
			return err
		}

		certPath := filepath.Join(n.Cfg.LabDir, "ssl", tlsCertFile)
		if err := clabutils.CreateFile(certPath, n.Config().TLSCert); err != nil {
			return err
		}

		caPath := filepath.Join(n.Cfg.LabDir, "ssl", tlsCAFile)
		if err := clabutils.CreateFile(caPath, n.Config().TLSAnchor); err != nil {
			return err
		}
	}
	return nil
}

func setMgmtInterface(node *clabtypes.NodeConfig) error {
	// use interface mapping file to set the Management interface if it is provided in the binds
	// section
	// default is Management0
	mgmtInterface := "Management0"
	for _, bindelement := range node.Binds {
		if !strings.Contains(bindelement, eosIntfMappingFile) {
			continue
		}

		bindsplit := strings.Split(bindelement, ":")
		if len(bindsplit) < 2 {
			return fmt.Errorf("malformed bind instruction: %s", bindelement)
		}

		var m []byte // byte representation of a map file
		m, err := os.ReadFile(bindsplit[0])
		if err != nil {
			return err
		}

		// Reset management interface if defined in the intfMapping file
		var intfMappingJson eosIntfMapping
		err = json.Unmarshal(m, &intfMappingJson)
		if err != nil {
			log.Debugf(
				"Management interface could not be read from intfMapping file for '%s' node.",
				node.ShortName,
			)
			return err
		}
		if v := intfMappingJson.ManagementIntf["eth0"]; v != "" {
			mgmtInterface = v
		}
	}
	log.Debugf("Management interface for '%s' node is set to %s.", node.ShortName, mgmtInterface)
	node.MgmtIntf = mgmtInterface

	return nil
}

// userProvidedIntfMapping reports whether the user already binds an
// EosIntfMapping.json, in which case containerlab must not generate one.
func userProvidedIntfMapping(node *clabtypes.NodeConfig) bool {
	for _, b := range node.Binds {
		if strings.Contains(b, eosIntfMappingFile) {
			return true
		}
	}
	return false
}

// dataIntfToEosName converts a containerlab cEOS data interface name (ethX, etX,
// or the ethX_Y[_Z] breakout shorthand) to its EOS interface name (EthernetX,
// EthernetX/Y or EthernetX/Y/Z). This reproduces the conversion the cEOS image
// performs itself when no interface mapping file is present, so that generating
// a mapping file does not lose the underscore shorthand.
func dataIntfToEosName(ifName string) (string, error) {
	m := dataIntfNameRegexp.FindStringSubmatch(ifName)
	if m == nil {
		return "", fmt.Errorf(
			"cannot map interface %q to an EOS interface name. "+
				"%s interface-map autogeneration supports ethX/etX and the "+
				"ethX_Y[_Z]/etX_Y[_Z] breakout shorthand; dotted subinterface names such as "+
				"eth1.100 are not supported (cEOS does not expose them via the interface "+
				"mapping file). Unset %s for this node, or configure subinterfaces "+
				"in the node's startup-config instead",
			ifName, intfMapEnvVar, intfMapEnvVar,
		)
	}

	name := "Ethernet" + m[1]
	if m[2] != "" {
		name += "/" + m[2]
	}
	if m[3] != "" {
		name += "/" + m[3]
	}
	return name, nil
}

// genIntfMapping reconciles the node's EosIntfMapping.json with the
// INTF_MAP_ETH0 environment variable. When set, it generates a mapping that maps
// eth0 to the requested management interface name and every data interface to
// its EOS name (preserving the ethX_Y -> EthernetX/Y shorthand, which a mapping
// file would otherwise disable), writes the file into the node's flash
// directory, sets the node's management interface name, and returns true.
//
// When INTF_MAP_ETH0 is unset (including the case where Init removed it because
// the user binds their own mapping file), it removes any mapping file left in
// the flash directory by a previous autogen run — so redeploying after unsetting
// the variable does not keep applying a stale map — and returns false, leaving
// the user-provided / Management0 handling in place. A user-provided map is
// bound from outside the flash directory, so the removal only targets a stale
// generated file.
func (n *ceos) genIntfMapping(node *clabtypes.NodeConfig) (bool, error) {
	mapPath := path.Join(node.LabDir, "flash", eosIntfMappingFile)

	mgmtName, ok := node.Env[intfMapEnvVar]
	if !ok {
		if err := os.Remove(mapPath); err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf(
				"failed to remove stale %s for node %q: %w",
				eosIntfMappingFile, node.ShortName, err,
			)
		}
		return false, nil
	}

	if !mgmtIntfNameRegexp.MatchString(mgmtName) {
		return false, fmt.Errorf(
			"node %q: %s value %q is not a valid management interface name (expected e.g. Management1)",
			node.ShortName, intfMapEnvVar, mgmtName,
		)
	}

	mapping := eosIntfMapping{
		ManagementIntf: map[string]string{"eth0": mgmtName},
		EthernetIntf:   map[string]string{},
	}
	for _, e := range n.Endpoints {
		ifName := e.GetIfaceName()
		if ifName == "eth0" {
			continue
		}
		eosName, err := dataIntfToEosName(ifName)
		if err != nil {
			return false, fmt.Errorf("node %q: %w", node.ShortName, err)
		}
		mapping.EthernetIntf[ifName] = eosName
	}

	b, err := json.MarshalIndent(mapping, "", "  ")
	if err != nil {
		return false, err
	}

	if err := clabutils.CreateFile(mapPath, string(b)); err != nil {
		return false, fmt.Errorf("failed to write %s for node %q: %w", eosIntfMappingFile, node.ShortName, err)
	}

	node.MgmtIntf = mgmtName
	log.Debugf("Management interface for '%s' node is set to %s (generated %s).",
		node.ShortName, mgmtName, eosIntfMappingFile)

	return true, nil
}

// ceosPostDeploy runs postdeploy actions which are required for ceos nodes.
func (n *ceos) ceosPostDeploy(_ context.Context) error {
	nodeCfg := n.Config()
	d, err := clabutils.SpawnCLIviaExec("arista_eos", nodeCfg.LongName, n.Runtime.GetName())
	if err != nil {
		return err
	}

	defer d.Close()

	cfgs := []string{
		"interface " + nodeCfg.MgmtIntf,
		"no ip address",
		"no ipv6 address",
	}

	// adding ipv4 address to configs
	if nodeCfg.MgmtIPv4Address != "" {
		cfgs = append(cfgs,
			fmt.Sprintf("ip address %s/%d", nodeCfg.MgmtIPv4Address, nodeCfg.MgmtIPv4PrefixLength),
		)
	}

	// adding ipv6 address to configs
	if nodeCfg.MgmtIPv6Address != "" {
		cfgs = append(
			cfgs,
			fmt.Sprintf(
				"ipv6 address %s/%d",
				nodeCfg.MgmtIPv6Address,
				nodeCfg.MgmtIPv6PrefixLength,
			),
		)
	}

	// configure data interfaces
	for _, e := range n.Endpoints {
		ifName := e.GetIfaceName()
		// skip management interface
		if ifName == nodeCfg.MgmtIntf {
			continue
		}

		v4 := e.GetIPv4Addr()
		v6 := e.GetIPv6Addr()

		if !v4.IsValid() && !v6.IsValid() {
			continue
		}

		cfgs = append(cfgs, "interface "+ifName)
		cfgs = append(cfgs, "no switchport")
		cfgs = append(cfgs, "no ip address")
		cfgs = append(cfgs, "no ipv6 address")

		if v4.IsValid() {
			cfgs = append(cfgs, fmt.Sprintf("ip address %s", v4.String()))
		}
		if v6.IsValid() {
			cfgs = append(cfgs, fmt.Sprintf("ipv6 address %s", v6.String()))
		}
	}

	// add save to startup cmd
	cfgs = append(cfgs, "wr")

	log.Debugf("cEOS PostDeploy configuration for node %s: %v", n.Cfg.ShortName, cfgs)

	resp, err := d.SendConfigs(cfgs)
	if err != nil {
		return err
	} else if resp.Failed != nil {
		return errors.New("failed CLI configuration")
	}

	return err
}

// CheckInterfaceName checks if a name of the interface referenced in the topology file correct.
func (n *ceos) CheckInterfaceName() error {
	// allow eth and et interfaces
	// https://regex101.com/r/umQW5Z/2
	ifRe := regexp.MustCompile(`eth[1-9][\w.]*$|et[1-9][\w.]*$`)
	for _, e := range n.Endpoints {
		if !ifRe.MatchString(e.GetIfaceName()) {
			return fmt.Errorf(
				"arista cEOS node %q has an interface named %q which doesn't match the required pattern. Interfaces should be named as ethX or etX, where X consists of alpanumerical characters",
				n.Cfg.ShortName,
				e.GetIfaceName(),
			)
		}
	}

	return nil
}
