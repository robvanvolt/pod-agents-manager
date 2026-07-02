# Homebrew formula for pod-agents-manager.
#
# This file lives in the project repo for review/versioning. To publish it,
# copy it into a tap repo named `homebrew-tap` under `Formula/` (see the
# README next to this file). Users then install with:
#
#     brew install robvanvolt/tap/pod-agents-manager
#     # or, before the first tagged release:
#     brew install --HEAD robvanvolt/tap/pod-agents-manager
#
class PodAgentsManager < Formula
  desc "Rootless Podman + Quadlet fleet manager for local AI coding agents"
  homepage "https://github.com/robvanvolt/pod-agents-manager"
  url "https://github.com/robvanvolt/pod-agents-manager/archive/refs/tags/v0.6.0.tar.gz"
  # Replace on each release: `shasum -a 256 v0.6.0.tar.gz`
  sha256 "84bc69d7acee49d9fc9e2405629e29ef9b18a215ea23033cd3c2b2bbaede9930"
  license "Apache-2.0"
  head "https://github.com/robvanvolt/pod-agents-manager.git", branch: "main"

  # Pure bash + an on-demand-built Go dashboard; no build deps. `bash` is
  # depended on so the formula uses a modern bash (macOS ships 3.2; the runtime
  # works on 3.2+, but 4+ is preferred for the dashboard/menus).
  depends_on "bash"

  def install
    # Ship the entrypoint and the whole config tree as read-only distribution
    # files under libexec. ~/.pod_agents_config is materialized from here on
    # first run (and refreshed on upgrade) by the entrypoint's bootstrap.
    libexec.install ".pod_agents" => "pod_agents"
    libexec.install ".pod_agents_config"

    # Thin wrapper on PATH. Points the entrypoint at the libexec distribution
    # dir and advertises the command name used in help text.
    (bin/"pod-agents").write <<~SH
      #!/bin/bash
      export POD_AGENTS_DIST_DIR="#{libexec}/.pod_agents_config"
      export POD_AGENTS_CMD_NAME="pod-agents"
      exec /bin/bash "#{libexec}/pod_agents" "$@"
    SH

    bash_completion.install "completions/pod-agents.bash" => "pod-agents"
  end

  def caveats
    <<~EOS
      pod-agents is installed as `pod-agents` (the `pod` name is left alone so
      it can't shadow CocoaPods). To use the shorter name, alias it:

          echo "alias pod=pod-agents" >> ~/.bashrc   # or ~/.zshrc

      First run materializes ~/.pod_agents_config/ (agents, flavors, skills,
      and your editable .env). Verify your host is ready with:

          pod-agents doctor

      The LAN dashboard (`pod-agents server start`) and pods require rootless
      Podman + systemd, i.e. a Linux host. On macOS this CLI manages a remote
      Linux host's fleet.
    EOS
  end

  test do
    assert_match "pod-agents-manager", shell_output("#{bin}/pod-agents --version")
    assert_match "Usage:", shell_output("#{bin}/pod-agents --help")
  end
end
