# Copyright 2026 Abhinav Gautam
# SPDX-License-Identifier: Apache-2.0
#
# Runs extrawest_ocpi's OCPI 2.2.1 application in both the CPO and the
# eMSP role with an in-memory store, for the evroaming interop tests.
# One store per module is shared by both roles: what a partner PUTs to
# the eMSP receiver is what the CPO sender lists, so objects make a round
# trip through the library's own schemas. Lines starting with "PEER "
# report what happened.

import os
import secrets
import time

import uvicorn
from py_ocpi import get_application
from py_ocpi.core.authentication.authenticator import Authenticator
from py_ocpi.core.crud import Crud
from py_ocpi.core.enums import Action, ModuleID, RoleEnum
from py_ocpi.modules.versions.enums import VersionNumber

TOKEN_A = os.environ.get("PEER_TOKEN_A", "peer-token-a")
HOST = os.environ["OCPI_HOST"]
STORE = {}
CLIENT = {}
TOKENS_C = set()


def say(*parts):
    print("PEER", *parts, flush=True)


def our_credentials(token):
    return {
        "token": token,
        "url": f"http://{HOST}/ocpi/versions",
        "roles": [
            {"role": "CPO", "party_id": "PER", "country_code": "BE",
             "business_details": {"name": "Peer CPO"}},
            {"role": "EMSP", "party_id": "PEM", "country_code": "BE",
             "business_details": {"name": "Peer eMSP"}},
        ],
    }


def key(module, id, kwargs):
    if module == ModuleID.hub_client_info:
        return f"{kwargs.get('country_code')}/{kwargs.get('party_id')}"
    return str(id)


class Auth(Authenticator):
    @classmethod
    async def get_valid_token_c(cls):
        return list(TOKENS_C)

    @classmethod
    async def get_valid_token_a(cls):
        return [TOKEN_A]


class Memory(Crud):
    @classmethod
    async def get(cls, module, role, id, *args, **kwargs):
        if module == ModuleID.credentials_and_registration:
            return our_credentials(id) if id in TOKENS_C else None
        if module == ModuleID.commands:
            say("command-result-asked", kwargs.get("command"))
            return {"result": "ACCEPTED"}
        if "response_url" in kwargs and "duration" not in kwargs:
            # The package's clear-profile task waits for an empty answer.
            say("clear-result-asked", kwargs.get("session_id"))
            return None
        if "duration" in kwargs:
            say("profile-result-asked", kwargs.get("session_id"))
            return {"result": "ACCEPTED", "profile": {
                "start_date_time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                # The package's ChargingProfile wants min_charge_rate and
                # one period object (2.2.1: min_charging_rate, optional,
                # and a list of periods).
                "charging_profile": {"charging_rate_unit": "W", "min_charge_rate": 0,
                                     "charging_profile_period": {"start_period": 0, "limit": 7400}}}}
        return STORE.get(module, {}).get(key(module, id, kwargs))

    @classmethod
    async def list(cls, module, role, filters, *args, **kwargs):
        items = list(STORE.get(module, {}).values())
        offset, limit = filters.get("offset", 0), filters.get("limit", 50)
        page = items[offset:offset + limit]
        say("list", module.value, role.value, len(items), offset, limit)
        return page, len(items), offset + limit >= len(items)

    @classmethod
    async def create(cls, module, role, data, *args, **kwargs):
        if module == ModuleID.credentials_and_registration:
            CLIENT.update(data["credentials"])
            CLIENT["endpoints"] = data["endpoints"]
            tok = secrets.token_hex(16)
            TOKENS_C.clear()
            TOKENS_C.add(tok)
            say("registered", data["credentials"]["url"], len(data["endpoints"]))
            return our_credentials(tok)
        k = key(module, data.get("id") or data.get("uid"), {**kwargs, **data})
        STORE.setdefault(module, {})[k] = data
        say("create", module.value, role.value, k)
        return data

    @classmethod
    async def update(cls, module, role, data, id, *args, **kwargs):
        if module == ModuleID.credentials_and_registration:
            CLIENT.update(data["credentials"])
            tok = secrets.token_hex(16)
            TOKENS_C.clear()
            TOKENS_C.add(tok)
            say("updated")
            return our_credentials(tok)
        if module == ModuleID.sessions and role == RoleEnum.cpo:
            say("preferences", id, data.get("profile_type"))
            return data
        if module in (ModuleID.commands, ModuleID.charging_profile):
            say("result", module.value, id, data)
            return data
        k = key(module, id, kwargs)
        old = STORE.setdefault(module, {}).get(k, {})
        old.update(data)
        STORE[module][k] = old
        say("update", module.value, role.value, k)
        return old

    @classmethod
    async def delete(cls, module, role, id, *args, **kwargs):
        if module == ModuleID.credentials_and_registration:
            TOKENS_C.clear()
            say("unregistered")
            return
        STORE.get(module, {}).pop(key(module, id, kwargs), None)
        say("delete", module.value, id)

    @classmethod
    async def do(cls, module, role, action, *args, data=None, **kwargs):
        if action == Action.get_client_token:
            return CLIENT.get("token")
        if action == Action.authorize_token:
            tok = STORE.get(ModuleID.tokens, {}).get(data["token_uid"])
            say("authorize", data["token_uid"])
            return {"allowed": "ALLOWED" if tok and tok.get("valid") else "BLOCKED",
                    "token": tok, "location": data.get("location_reference")}
        say("do", module.value, action.value)
        return {"result": "ACCEPTED", "timeout": 30}


app = get_application(
    version_numbers=[VersionNumber.v_2_2_1],
    roles=[RoleEnum.cpo, RoleEnum.emsp],
    crud=Memory,
    modules=[ModuleID.credentials_and_registration, ModuleID.locations, ModuleID.sessions,
             ModuleID.cdrs, ModuleID.tariffs, ModuleID.tokens, ModuleID.commands,
             ModuleID.charging_profile, ModuleID.hub_client_info],
    authenticator=Auth,
    http_push=True,
)

if __name__ == "__main__":
    say("ready")
    uvicorn.run(app, host="0.0.0.0", port=8000, log_level="warning")
