# Git Credential

## Prerequisites

- An account on the Git hosting service (GitHub, GitLab, Bitbucket, etc.).
- A Personal Access Token (PAT) with the required scopes.

### Creating a token

**GitHub:**
1. Go to **Settings → Developer settings → Personal access tokens → Tokens (classic)**.
2. Create a token with the required scopes (e.g. `repo`).

**GitLab:**
1. Go to **User Settings → Access Tokens**.
2. Create a token with the required scopes (e.g. `read_api`, `read_repository`).

## Fields

| Field | Description |
|------|-------------|
| Git Host | Hostname of the Git server (e.g. `github.com`, `gitlab.com`). |
| Username | Username. For GitLab with a PAT: `oauth2`. For GitHub: your GitHub username. |
| Personal Access Token | Your PAT (format: `glpat-...` for GitLab, `ghp_...` for GitHub). Stored in the Keychain. |

## After creating

Click **Test** to verify that the token is present in the secret store. No network test is performed.

## Notes

- Git credentials are stored passively and used automatically during git operations through mux.
- Create a separate connection per Git host.
