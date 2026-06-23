variable "image_tag" {
  type        = string
  default     = "latest"
  description = "Docker image tag for ATC"
}

variable "consul_address" {
  type        = string
  default     = "http://127.0.0.1:8500"
  description = "The address of the Consul agent ATC should communicate with"
}

variable "consul_dc" {
  type        = string
  default     = "dc1"
  description = "Target datacenter in Consul"
}

variable "consul_namespace" {
  type        = string
  default     = ""
  description = "Target namespace in Consul Enterprise"
}

variable "write_rate_limit" {
  type        = string
  default     = "1s"
  description = "Coalescing rate limit window"
}

variable "replica_count" {
  type        = number
  default     = 2
  description = "Number of replica instances to run for Active-Passive HA"
}

variable "dry_run" {
  type        = bool
  default     = false
  description = "Enable dry-run mode for catalog reconciliation"
}

job "atc" {
  datacenters = ["dc1"]
  type        = "service"

  group "atc" {
    count = var.replica_count

    network {
      port "http" {
        to = 8088
      }
      port "metrics" {
        to = 8089
      }
    }

    service {
      name = "atc"
      port = "http"
      tags = ["atc", "traffic-control"]

      check {
        type     = "http"
        path     = "/ready"
        interval = "10s"
        timeout  = "2s"
      }
    }

    service {
      name = "atc-metrics"
      port = "metrics"
      tags = ["prometheus", "metrics"]

      check {
        type     = "http"
        path     = "/metrics"
        interval = "10s"
        timeout  = "2s"
      }
    }

    task "atc" {
      driver = "docker"

      config {
        image = "atcprojectio/atc:${var.image_tag}"
        ports = ["http", "metrics"]
        args = [
          "server",
          "--config", "local/atc-config.yaml"
        ]
      }

      env {
        ATC_PORT             = "${NOMAD_PORT_http}"
        ATC_METRICS_PORT     = "${NOMAD_PORT_metrics}"
        ATC_CONSUL_ADDR      = "${var.consul_address}"
        ATC_CONSUL_DC        = "${var.consul_dc}"
        ATC_CONSUL_NAMESPACE = "${var.consul_namespace}"
        ATC_WRITE_RATE_LIMIT = "${var.write_rate_limit}"
        ATC_LOG_LEVEL        = "info"
      }

      template {
        data = <<EOF
service: "atc-service"

ha:
  enabled: true
  lock_key: "atc/leader/lock"
  session_ttl: "15s"

auth:
  enabled: false
  static_keys:
    - "atc-super-secret-token"
  consul_token_delegation: true

dampening_period: "5s"
min_dampening_period: "1s"
consul_namespace: "${var.consul_namespace}"
write_rate_limit: "${var.write_rate_limit}"
dry_run: ${var.dry_run}

strategies:
  failover:
    standard-failover:
      connect_timeout: "15s"
      targets:
        - service: "payment-service"
          datacenter: "dc2"
  redirect:
    standard-redirect:
      service: "payment-service"
      datacenter: "dc2"
EOF
        destination = "local/atc-config.yaml"
        change_mode = "restart"
      }

      resources {
        cpu    = 200
        memory = 128
      }
    }
  }
}
