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

variable "replica_count" {
  type        = number
  default     = 2
  description = "Number of replica instances to run for Active-Passive HA"
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
        ATC_PORT         = "${NOMAD_PORT_http}"
        ATC_METRICS_PORT = "${NOMAD_PORT_metrics}"
        ATC_CONSUL_ADDR  = "${var.consul_address}"
        ATC_CONSUL_DC    = "${var.consul_dc}"
        ATC_LOG_LEVEL    = "info"
      }

      template {
        data = <<EOF
service: "atc-service"

ha:
  enabled: true
  lock_key: "atc/leader/lock"
  session_ttl: "15s"

dampening_period: "5s"
min_dampening_period: "1s"

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
