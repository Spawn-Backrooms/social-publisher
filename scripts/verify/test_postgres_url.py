"""Regressions for routes that libpq would otherwise override implicitly."""

import unittest
from urllib.parse import urlencode, urlunsplit

from postgres_url import explicit_connection_environment, validated_url


class PostgreSQLFixtureRoutes(unittest.TestCase):
    def test_routing_and_schema_query_overrides_are_rejected(self):
        for key, value in (
            ("host", "192.0.2.10"), ("host", "127.0.0.1,192.0.2.10"),
            ("hostaddr", "192.0.2.10"), ("port", "5433"),
            ("dbname", "existing_database"), ("database", "existing_database"),
            ("user", "another_role"), ("password", "fixture"),
            ("service", "external"), ("servicefile", "/tmp/service.conf"),
            ("options", "-c search_path=public"), ("search_path", "public"),
        ):
            with self.subTest(key=key, value=value):
                with self.assertRaises(ValueError):
                    validated_url("postgres://publisher@127.0.0.1/postgres?" + urlencode({key: value}), local_only=True)

    def test_loopback_and_security_options_preserve_generated_database(self):
        for host in ("127.0.0.1", "localhost", "[::1]"):
            with self.subTest(host=host):
                parsed = validated_url("postgres://publisher@" + host + "/postgres?sslmode=disable&connect_timeout=5", local_only=True)
                target = validated_url(urlunsplit(parsed._replace(path="/sp_verify_restore_target_fixture")), local_only=True)
                self.assertEqual(target.path, "/sp_verify_restore_target_fixture")
                self.assertEqual(target.netloc, parsed.netloc)

    def test_remote_fallback_duplicate_options_and_fragments_rejected(self):
        for value in (
            "postgres://publisher@192.0.2.10/postgres",
            "postgres://publisher@127.0.0.1,192.0.2.10/postgres",
            "postgres://publisher@127.0.0.1/postgres?sslmode=disable&sslmode=require",
            "postgres://publisher@127.0.0.1/postgres#routing",
        ):
            with self.subTest(value=value), self.assertRaises(ValueError):
                validated_url(value, local_only=True)

    def test_inherited_connection_defaults_cannot_route_fixture_clients(self):
        environment = explicit_connection_environment({
            "PGHOST": "192.0.2.10", "PGHOSTADDR": "192.0.2.11", "PGPORT": "5433",
            "PGDATABASE": "existing_database", "PGSERVICE": "external",
            "PGSERVICEFILE": "/tmp/service.conf", "PGOPTIONS": "-c search_path=public",
            "PGPASSWORD": "fixture", "PG_BIN": "/client/bin", "PATH": "/bin",
        })
        self.assertEqual(environment, {"PG_BIN": "/client/bin", "PATH": "/bin"})


if __name__ == "__main__":
    unittest.main()
