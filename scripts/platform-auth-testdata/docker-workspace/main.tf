# Docker port of the AKS/Kubernetes workspace template, for LOCAL testing of the
# platform-cookie auth bridge. It preserves the pieces that matter for that
# test and matches the production template's shape:
#
#   * a coder_agent "main" that installs and runs code-server on :13337
#   * a coder_app "code-server" with share = "owner"  (path app, subdomain = false)
#   * git user config sourced from the workspace owner
#
# Deliberately TRIMMED from the production template because it is AKS/backend
# specific and would hang or fail on a laptop (and is irrelevant to the auth
# test):
#   * the kubernetes provider / Deployment / PVC   -> replaced with docker
#   * the git credential helper that calls BACKEND_BASE_URL
#   * the GitHub CLI wrapper and docker-in-docker (dockerd) install
#   * the VS Code extension download (which also carried a hardcoded GitHub
#     PAT in the production template -- that token should be revoked and moved
#     to a secret regardless of this file)
#   * repository cloning (needs the backend token; skipped locally)

terraform {
  required_providers {
    coder  = { source = "coder/coder" }
    docker = { source = "kreuzwerker/docker" }
  }
}

variable "docker_socket" {
  default     = ""
  description = "(Optional) Docker socket URI"
  type        = string
}

provider "docker" {
  host = var.docker_socket != "" ? var.docker_socket : null
}

provider "coder" {}

data "coder_provisioner" "me" {}
data "coder_workspace" "me" {}
data "coder_workspace_owner" "me" {}

# Optional, mirrors the production template so the opened folder matches.
data "coder_parameter" "github_repo" {
  name         = "github_repo"
  display_name = "GitHub Repository"
  description  = "Folder to open in code-server (cloning is skipped in the local port)."
  type         = "string"
  mutable      = true
  default      = ""
}

resource "coder_agent" "main" {
  os   = "linux"
  arch = data.coder_provisioner.me.arch

  display_apps {
    web_terminal = false
    ssh_helper   = false
  }

  startup_script = <<-EOT
    set -e

    # Git identity from the workspace owner (same as the production template).
    git config --global user.name  "${data.coder_workspace_owner.me.name}"
    git config --global user.email "${data.coder_workspace_owner.me.email}"
    git config --global init.defaultBranch main

    # Install and launch code-server on :13337, matching the production template.
    echo "=== Code-server ==="
    curl -fsSL https://code-server.dev/install.sh | sh -s -- --method=standalone --prefix=/tmp/code-server

    /tmp/code-server/bin/code-server \
      --auth none --host 127.0.0.1 --port 13337 \
      >/tmp/code-server.log 2>&1 &
  EOT

  metadata {
    display_name = "CPU Usage"
    key          = "0_cpu_usage"
    script       = "coder stat cpu"
    interval     = 10
    timeout      = 1
  }
  metadata {
    display_name = "RAM Usage"
    key          = "1_ram_usage"
    script       = "coder stat mem"
    interval     = 10
    timeout      = 1
  }
}

# code-server, owner-shared -- this is what the auth bridge must let the owning
# member reach directly. Identical shape to the production template.
resource "coder_app" "code-server" {
  agent_id     = coder_agent.main.id
  slug         = "code-server"
  display_name = "code-server"
  icon         = "/icon/code.svg"
  url          = "http://localhost:13337?folder=${data.coder_parameter.github_repo.value != "" ? "/home/coder/${data.coder_parameter.github_repo.value}" : "/home/coder"}"
  subdomain    = false
  share        = "owner"

  healthcheck {
    url       = "http://localhost:13337/healthz"
    interval  = 3
    threshold = 10
  }
}

resource "docker_volume" "home_volume" {
  name = "coder-${data.coder_workspace.me.id}-home"
  lifecycle {
    ignore_changes = all
  }
}

resource "docker_container" "workspace" {
  count      = data.coder_workspace.me.start_count
  image      = "codercom/enterprise-base:ubuntu"
  name       = "coder-${data.coder_workspace_owner.me.name}-${lower(data.coder_workspace.me.name)}"
  hostname   = data.coder_workspace.me.name
  entrypoint = ["sh", "-c", replace(coder_agent.main.init_script, "/localhost|127\\.0\\.0\\.1/", "host.docker.internal")]
  env        = ["CODER_AGENT_TOKEN=${coder_agent.main.token}"]
  host {
    host = "host.docker.internal"
    ip   = "host-gateway"
  }
  volumes {
    container_path = "/home/coder"
    volume_name    = docker_volume.home_volume.name
    read_only      = false
  }
}
