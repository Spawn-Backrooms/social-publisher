"""Validate explicit fixture routes shared by pgx and libpq clients."""

import ipaddress
from urllib.parse import parse_qsl, urlsplit


# Routing and PostgreSQL runtime parameters must not override the fixture
# database or schema. The generated URL supplies those fields itself.
CONNECTION_OPTIONS = {
    "sslmode", "sslrootcert", "sslcert", "sslkey", "sslpassword",
    "ssl_min_protocol_version", "ssl_max_protocol_version", "sslnegotiation",
    "channel_binding", "gssencmode", "connect_timeout", "application_name",
    "target_session_attrs", "keepalives", "keepalives_idle",
    "keepalives_interval", "keepalives_count", "tcp_user_timeout",
}


def validated_url(value, *, local_only=False):
    try:
        parsed = urlsplit(value)
        hostname = parsed.hostname
        port = parsed.port
    except ValueError:
        raise ValueError("TEST_DATABASE_URL is not a valid explicit PostgreSQL URL") from None
    if parsed.scheme not in {"postgres", "postgresql"} or not hostname or parsed.fragment:
        raise ValueError("TEST_DATABASE_URL must be an explicit PostgreSQL URL without a fragment")
    if port is not None and not 1 <= port <= 65535:
        raise ValueError("TEST_DATABASE_URL has an invalid PostgreSQL port")
    # A single authority keeps Python, pgx, and libpq on the same route. A
    # multihost URL, service file, hostaddr, or query dbname cannot redirect it.
    if "," in hostname:
        raise ValueError("Fixture verification requires one explicit PostgreSQL host")
    options = parse_qsl(parsed.query, keep_blank_values=True)
    keys = [key for key, _ in options]
    if len(keys) != len(set(keys)) or any(key not in CONNECTION_OPTIONS for key in keys):
        raise ValueError("TEST_DATABASE_URL query must contain only unique TLS/connect options; routing and runtime overrides are forbidden")
    if local_only and hostname != "localhost":
        try:
            local = ipaddress.ip_address(hostname).is_loopback
        except ValueError:
            local = False
        if not local:
            raise ValueError("Restore smoke fixtures require a loopback PostgreSQL host")
    return parsed


def explicit_connection_environment(environment):
    # Both clients also read connection defaults from PG* variables. The URL
    # is the only connection authority for a disposable fixture. PG_BIN only
    # selects local utilities and is not a PostgreSQL connection parameter.
    return {key: value for key, value in environment.items() if not key.startswith("PG") or key == "PG_BIN"}
