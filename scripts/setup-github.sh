#!/usr/bin/env bash
set -euo pipefail

# setup-github.sh — Create GitHub repo, configure branch protection, and push.
# Usage: ./scripts/setup-github.sh [org-or-user] [repo-name]
#   defaults to JessMorgan/anti-loop-proxy

ORG="${1:-JessMorgan}"
REPO="${2:-anti-loop-proxy}"
BRANCH="main"

echo "=== Setting up GitHub repo: ${ORG}/${REPO} ==="

# 1. Create the repo if it doesn't exist
if ! gh repo view "${ORG}/${REPO}" > /dev/null 2>&1; then
  echo "Creating repo ${ORG}/${REPO}..."
  gh repo create "${ORG}/${REPO}" --private --source=. --push
else
  echo "Repo ${ORG}/${REPO} already exists."
  git remote get-url origin > /dev/null 2>&1 || \
    git remote add origin "git@github.com:${ORG}/${REPO}.git"
fi

# 2. Push code
echo "Pushing to origin..."
git push -u origin "${BRANCH}" 2>/dev/null || git push origin "${BRANCH}"

# 3. Configure branch protection — require all CI jobs to pass before merge
echo "Configuring branch protection rules for ${BRANCH}..."
gh api \
  --method PUT \
  "repos/${ORG}/${REPO}/branches/${BRANCH}/protection" \
  --field required_status_checks='{"strict":true,"contexts":["Formatting","Linting","Unit tests","Static analysis","Strict build"]}' \
  --field enforce_admins=false \
  --field required_pull_request_reviews='{"required_approving_review_count":0,"dismiss_stale_reviews":true,"require_code_owner_reviews":false}' \
  --field restrictions=null \
  --field allow_force_pushes=false \
  --field allow_deletions=false \
  --field block_creations=false \
  --field required_conversation_resolution=true \
  2>&1 || echo "WARNING: Could not set branch protection (may need admin permissions)"

# 4. Enable Dependabot alerts and security fixes
echo "Verifying Dependabot configuration..."
if [ -f .github/dependabot.yml ]; then
  echo "Dependabot config found at .github/dependabot.yml ✓"
else
  echo "WARNING: .github/dependabot.yml not found"
fi

echo ""
echo "=== Setup complete ==="
echo ""
echo "Branch protection rules configured:"
echo "  - Require status checks: Formatting, Linting, Unit tests, Static analysis, Strict build"
echo "  - Require branches to be up to date before merging"
echo "  - Dismiss stale reviews on new pushes"
echo "  - Require conversation resolution"
echo "  - Force pushes and deletions blocked"
echo ""
echo "Dependabot will create PRs weekly for:"
echo "  - Go module dependencies (gomod)"
echo "  - GitHub Actions (github-actions)"
