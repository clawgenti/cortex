"""Tests for keycloak_sync.py."""

import importlib.util
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

# Load keycloak_sync via importlib to avoid mutating sys.path
_spec = importlib.util.spec_from_file_location(
    "keycloak_sync",
    Path(__file__).resolve().parents[1] / "scripts" / "keycloak_sync.py",
)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

KeycloakReconciler = _mod.KeycloakReconciler
ReconcileResult = _mod.ReconcileResult
RouteTarget = _mod.RouteTarget
load_routes = _mod.load_routes
print_summary = _mod.print_summary


# ---------------------------------------------------------------------------
# Tests: RouteTarget
# ---------------------------------------------------------------------------


class TestRouteTarget:
    def test_defaults(self):
        rt = RouteTarget(host="svc.example.com", audience="aud", scopes=["openid"])
        assert rt.passthrough is False

    def test_passthrough(self):
        rt = RouteTarget(host="svc.example.com", audience="aud", scopes=[], passthrough=True)
        assert rt.passthrough is True


# ---------------------------------------------------------------------------
# Tests: load_routes
# ---------------------------------------------------------------------------


class TestLoadRoutes:
    def test_loads_valid_yaml(self, tmp_path):
        routes_file = tmp_path / "routes.yaml"
        routes_file.write_text(
            """
- host: "svc-a.example.com"
  target_audience: "audience-a"
  token_scopes: "openid audience-a-aud"
- host: "svc-b.example.com"
  target_audience: "audience-b"
  passthrough: true
"""
        )

        targets = load_routes(str(routes_file))

        assert len(targets) == 2
        assert targets[0].host == "svc-a.example.com"
        assert targets[0].audience == "audience-a"
        assert targets[0].scopes == ["openid", "audience-a-aud"]
        assert targets[0].passthrough is False
        assert targets[1].passthrough is True

    def test_skips_routes_without_audience(self, tmp_path):
        routes_file = tmp_path / "routes.yaml"
        routes_file.write_text(
            """
- host: "svc-a.example.com"
- host: "svc-b.example.com"
  target_audience: "audience-b"
"""
        )

        targets = load_routes(str(routes_file))
        assert len(targets) == 1
        assert targets[0].audience == "audience-b"

    def test_empty_file_returns_empty(self, tmp_path):
        routes_file = tmp_path / "routes.yaml"
        routes_file.write_text("")

        targets = load_routes(str(routes_file))
        assert targets == []


# ---------------------------------------------------------------------------
# Tests: KeycloakReconciler
# ---------------------------------------------------------------------------


class TestKeycloakReconciler:
    def test_skips_passthrough_targets(self):
        kc = MagicMock()
        reconciler = KeycloakReconciler(kc, dry_run=True, auto_yes=True)

        targets = [
            RouteTarget(host="svc.example.com", audience="aud", scopes=[], passthrough=True)
        ]
        result = reconciler.reconcile(targets)

        assert result.targets_checked == 0
        kc.get_client_id.assert_not_called()

    def test_dry_run_creates_client(self):
        kc = MagicMock()
        kc.get_client_id.return_value = None
        reconciler = KeycloakReconciler(kc, dry_run=True, auto_yes=True)

        targets = [
            RouteTarget(host="svc.example.com", audience="my-target", scopes=[])
        ]
        result = reconciler.reconcile(targets)

        assert result.targets_checked == 1
        # In dry run, create_client should NOT be called on keycloak
        kc.create_client.assert_not_called()

    def test_existing_client_is_ok(self):
        kc = MagicMock()
        kc.get_client_id.return_value = "existing-uuid"
        kc.get_client.return_value = {
            "attributes": {"authbridge.hostname": "svc.example.com"}
        }
        reconciler = KeycloakReconciler(kc, dry_run=False, auto_yes=True)

        targets = [
            RouteTarget(host="svc.example.com", audience="my-target", scopes=[])
        ]
        result = reconciler.reconcile(targets)

        assert result.targets_checked == 1
        assert result.clients_created == 0

    def test_hostname_mismatch_detected(self):
        kc = MagicMock()
        kc.get_client_id.return_value = "uuid"
        kc.get_client.return_value = {
            "attributes": {"authbridge.hostname": "old-host.example.com"}
        }
        reconciler = KeycloakReconciler(kc, dry_run=False, auto_yes=True)

        targets = [
            RouteTarget(host="new-host.example.com", audience="my-target", scopes=[])
        ]
        result = reconciler.reconcile(targets)

        assert result.hostnames_set == 1
        kc.update_client.assert_called_once()

    def test_scope_creation_with_audience_mapper(self):
        kc = MagicMock()
        kc.get_client_id.return_value = "uuid"
        kc.get_client.return_value = {
            "attributes": {"authbridge.hostname": "svc.example.com"}
        }

        # get_client_scopes is called multiple times:
        # 1. _check_scopes: initial check — not found
        # 2. _create_scope_with_mapper -> _find_scope: after creation — found
        # 3. _check_scopes -> _find_scope (for assign): scope lookup again
        new_scope = {"name": "my-target-aud", "id": "scope-456"}
        kc.get_client_scopes.side_effect = [
            [],           # First call: scope not found
            [new_scope],  # Second call: after creation
            [new_scope],  # Third call: any subsequent lookup
        ]

        reconciler = KeycloakReconciler(kc, dry_run=False, auto_yes=True)

        targets = [
            RouteTarget(
                host="svc.example.com",
                audience="my-target",
                scopes=["my-target-aud"],
            )
        ]
        result = reconciler.reconcile(targets)

        assert result.scopes_created == 1
        kc.create_client_scope.assert_called_once()


# ---------------------------------------------------------------------------
# Tests: ReconcileResult
# ---------------------------------------------------------------------------


class TestReconcileResult:
    def test_defaults_are_zero(self):
        r = ReconcileResult()
        assert r.targets_checked == 0
        assert r.clients_created == 0
        assert r.errors == 0
        assert r.agent_client_created is False


# ---------------------------------------------------------------------------
# Tests: print_summary (smoke test)
# ---------------------------------------------------------------------------


class TestPrintSummary:
    def test_does_not_raise(self, capsys):
        result = ReconcileResult(
            targets_checked=3,
            clients_created=1,
            scopes_created=2,
            errors=0,
        )
        print_summary(result)

        captured = capsys.readouterr()
        assert "3 targets checked" in captured.out
        assert "1 clients created" in captured.out
        assert "2 scopes created" in captured.out
