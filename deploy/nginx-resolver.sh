#!/bin/sh
# Point nginx at the container runtime's DNS server, so it can RE-resolve the `api` upstream
# instead of caching one set of replica IPs from start-up (see the comment in deploy/nginx.conf).
#
# The resolver has to be an IP literal, and which IP it is depends on the runtime (Podman's
# aardvark-dns, Docker's embedded 127.0.0.11, a plain host resolver), so it can't be baked into the
# config - it's read from the container's own /etc/resolv.conf at start-up. The nginx image runs
# every /docker-entrypoint.d/*.sh before starting, which is where this lands.
#
# If there's no nameserver to find, the placeholder is left as a comment: nginx then keeps its
# start-up-only resolution, which is exactly the old behaviour and fine for a single api replica.
#
# It also points the portal at the API: KAAS_API_UPSTREAM overrides the `api:8080` the config ships
# with, because the service is not called `api` everywhere - under the Helm chart (deploy/helm) it
# is a Kubernetes Service named after the release.
set -eu

CONF=/etc/nginx/conf.d/default.conf

if [ -n "${KAAS_API_UPSTREAM:-}" ]; then
    sed -i "s|set \$api_upstream .*;|set \$api_upstream ${KAAS_API_UPSTREAM};|" "$CONF"
    echo "$0: api upstream set to ${KAAS_API_UPSTREAM}"
fi

NS=$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf 2>/dev/null || true)

if [ -z "${NS:-}" ]; then
    echo "$0: no nameserver in /etc/resolv.conf - leaving nginx with start-up DNS resolution"
    exit 0
fi

# IPv6 nameservers must be bracketed in an nginx resolver directive.
case "$NS" in
*:*) NS="[$NS]" ;;
esac

# valid=10s: how long a resolved upstream is cached. Short, so a replica that comes or goes is
# picked up in seconds; not zero, so a healthy request path isn't a DNS lookup every time.
# ipv6=off: the compose network is v4, and querying AAAA only adds failed lookups.
sed -i "s|#__RESOLVER__|resolver ${NS} valid=10s ipv6=off;|" "$CONF"
echo "$0: nginx resolver set to ${NS}"
