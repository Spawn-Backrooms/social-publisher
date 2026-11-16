# Security policy

Report vulnerabilities through GitHub private vulnerability reporting:

https://github.com/Spawn-Backrooms/social-publisher/security/advisories/new

Don't open a public issue, and don't include tokens, provider credentials or real account data in a report. Please test only against your own deployment and your own provider accounts.

In scope: account connection and OAuth handling, token encryption and storage, tenant and user isolation, media fetching (SSRF), scheduling and publication integrity.

This repository holds no production keys or provider app secrets. `keygen` generates all secrets locally.
