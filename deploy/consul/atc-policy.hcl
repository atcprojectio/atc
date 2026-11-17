# Least-privilege Consul ACL policy for ATC's own token (consul_token / ATC_CONSUL_TOKEN).
# Verified against a default-deny Consul by pkg/atc/integration/acl_policy_test.go; every
# rule is required.
#
#   consul acl policy create -name atc -rules @deploy/consul/atc-policy.hcl
#   consul acl token create -description "ATC" -policy-name atc

# Read and write service-resolver config entries. Consul requires service:write on a service
# to change its resolver. This also allows registering services under those names; if your
# ATC-managed services share a naming convention, narrow the prefix accordingly.
service_prefix "" {
  policy = "write"
}

# Read the service catalog (without node:read Consul hides services from ATC instead of
# failing) and datacenter membership for /api/federation.
node_prefix "" {
  policy = "read"
}

# Detect the local datacenter and check delegated tokens (agent/self).
agent_prefix "" {
  policy = "read"
}

# HA leader election: the lock keys under ha.lock_key (default "atc/leader/").
key_prefix "atc/leader/" {
  policy = "write"
}

# Sessions that hold the leader locks; also needed by "atc leader force-unlock".
session_prefix "" {
  policy = "write"
}
