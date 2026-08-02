// Package hostagent exposes the host-agent assets the fuse cli embeds for
// `fuse local`: the firecracker agent and the local-stack setup script. The
// agents themselves are python/shell and run on hosts, not in this process;
// embedding them lets a released fuse binary bring up a local stack without
// fetching repo files over the network.
package hostagent

import _ "embed"

// FCAgentPy is the firecracker host agent (host-agent/firecracker/fc-agent.py),
// byte-exact.
//
//go:embed firecracker/fc-agent.py
var FCAgentPy []byte

// LocalSetupSh is the fuse local stack installer/runner
// (host-agent/local/fuse-local-setup.sh), byte-exact.
//
//go:embed local/fuse-local-setup.sh
var LocalSetupSh []byte
