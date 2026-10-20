# Rotating a Domain's federation key

Peers pin each Domain's Ed25519 key on first contact (RFC 4.3). Rotating
replaces it without any peer noticing, as long as the old key stays published
as a previous key for a while (RFC 9.5).

## Steps

1. Generate the new key next to the old one:

       openssl genpkey -algorithm ed25519 -out keys/example.com.ed25519.2026-11.pem

2. In the Domain's `federation.json`, point `privateKeyFile` at the new key and
   add the old one to `previousKeys` with the time you make the change:

       "privateKeyFile": "/etc/webmail/keys/example.com.ed25519.2026-11.pem",
       "previousKeys": [
         {"file": "/etc/webmail/keys/example.com.ed25519.pem", "retiredAt": "2026-11-02T09:00:00Z"}
       ]

3. Restart the API and the worker of the Domain. From now on every envelope is
   signed with the new key, and the descriptor publishes the old key under
   `previous_public_keys`.

4. Peers notice by themselves: a signature they cannot verify with the key
   they pinned makes them fetch the descriptor again (RFC 4.3).

5. After 48 hours the server stops publishing the old key, even if the config
   still names it; publishing it longer is forbidden. Remove the entry from
   `previousKeys` and delete the old key file at your next change.

## Notes

- `retiredAt` is required. Without it the server cannot tell when the window
  ends, so it refuses to start.
- At most four previous keys may be listed (RFC 4.2).
- Rotating again within 48 hours is allowed: list both old keys, each with its
  own `retiredAt`.
- If a key leaked, do not keep it as a previous key: rotate with an empty
  `previousKeys`. Peers then refuse anything signed with the old key at once,
  and refresh to the new one on the first envelope signed with it.
