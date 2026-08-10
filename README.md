# location-agent

Prebuilt binaries of the agent that turns a Mac into a location the agent
platform can place runtimes on. The agent watches the platform for runtimes
assigned to this machine and materialises each one as a local docker container.

Only the built artifacts live here — the source is in a private repository.

## Install

```bash
agentctl connect <string from the platform>
```

The string carries everything the agent needs: where the control plane is, how
to authenticate to it, and which location this machine is. `connect` downloads
the binary matching this machine, installs it under `~/.location-agent`, and
registers it with launchd so it starts at login and restarts on failure.

```bash
agentctl connect status     # location, path, state, pid
agentctl disconnect         # unregister from launchd
```

## Releases

Each release publishes one binary per platform, named
`location-agent-<os>-<arch>` — `darwin-arm64` and `darwin-amd64` today. A
release tag is what `connect` installs when the platform pins a version;
without one it takes the latest.
