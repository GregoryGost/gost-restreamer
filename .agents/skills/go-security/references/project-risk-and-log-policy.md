# Project-selected risk and log disclosure

## Select once during skill integration

The target owner records the **single** Go Application Context And Data Disclosure Policy in `CODEX_PROJECT.md` when installing the relevant Go profiles. This establishes application role, network reachability, trusted actors, protected assets, accepted risks, security controls, log audiences and data categories. Agent tasks use it directly rather than redoing a risk evaluation every time.

| Selected role | Possible requirements | Avoid without project evidence |
| --- | --- | --- |
| Owner-operated personal-local tool | Simple local operation, full-value diagnostics if approved | Public auth endpoints, cloud IAM, mandatory remote log service |
| Internal service | Defined actor roles, transport and data boundaries | Externally exposed-service assumptions |
| Public endpoint | Input, authorization and resource-abuse controls appropriate to stated threats | Unrequested vendor infrastructure and universal controls |

A local application can contact remote services. This is a fact to record during initial policy selection, not proof that an owner-chosen local logging policy is prohibited.

## Logging policies

- `full` means preserve exact values of *designated* fields in the approved application-owned sink, including sensitive request or authentication fields if the owner has explicitly selected those categories. The agent must not silently redact, hash or truncate them.
- `masked` follows the project's actual masking/omission rule, not an invented generic algorithm.
- `selective` follows the declared per-category mapping. Missing entries relevant to a task are unresolved profile values.
- Data destinations are distinct: local logs, an external collector, CI artifacts and an agent's GitHub comment do not inherit one another's disclosure permissions. Full-value local logs do not authorize sending the contents to a third-party service or repeating them in the assistant's response.

Treat the owner's selected tradeoff as part of the project contract. Report objectively verified contradictions (e.g. a claimed local-only sink now exports data) and request an **explicit profile revision**, not a unilateral security rewrite.

