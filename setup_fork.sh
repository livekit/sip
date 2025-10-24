#!/bin/bash

# Script to setup fork and push branch for pull request
# Usage: ./setup_fork.sh YOUR_GITHUB_USERNAME

if [ -z "$1" ]; then
    echo "Usage: ./setup_fork.sh YOUR_GITHUB_USERNAME"
    echo ""
    echo "Example: ./setup_fork.sh andrewbull"
    exit 1
fi

USERNAME=$1
BRANCH="feature/rfc-4028-session-timers"

echo "🔧 Setting up fork for pull request..."
echo ""

# Check if we're in the right directory
if [ ! -d ".git" ]; then
    echo "❌ Error: Not in a git repository"
    exit 1
fi

# Check if branch exists
if ! git show-ref --verify --quiet refs/heads/$BRANCH; then
    echo "❌ Error: Branch $BRANCH doesn't exist"
    exit 1
fi

# Check current branch
CURRENT_BRANCH=$(git branch --show-current)
if [ "$CURRENT_BRANCH" != "$BRANCH" ]; then
    echo "📌 Switching to branch $BRANCH..."
    git checkout $BRANCH
fi

# Check if fork remote already exists
if git remote | grep -q "^fork$"; then
    echo "✅ Fork remote already exists"
    git remote -v | grep fork
else
    echo "➕ Adding fork remote..."

    # Ask user to choose HTTPS or SSH
    echo ""
    echo "Choose authentication method:"
    echo "  1) HTTPS (use GitHub username/password or token)"
    echo "  2) SSH (use SSH key)"
    read -p "Enter choice [1 or 2]: " AUTH_CHOICE

    if [ "$AUTH_CHOICE" = "2" ]; then
        FORK_URL="git@github.com:$USERNAME/sip.git"
    else
        FORK_URL="https://github.com/$USERNAME/sip.git"
    fi

    git remote add fork $FORK_URL
    echo "✅ Added fork remote: $FORK_URL"
fi

echo ""
echo "📊 Current remotes:"
git remote -v

echo ""
echo "🚀 Pushing branch to fork..."
if git push -u fork $BRANCH; then
    echo ""
    echo "✅ Successfully pushed to fork!"
    echo ""
    echo "📝 Next steps:"
    echo "  1. Go to: https://github.com/$USERNAME/sip"
    echo "  2. Click 'Compare & pull request'"
    echo "  3. Fill in PR details (see PR_DESCRIPTION.md)"
    echo "  4. Submit PR to livekit/sip"
    echo ""
    echo "🎉 You're ready to create the pull request!"
else
    echo ""
    echo "❌ Push failed!"
    echo ""
    echo "Common issues:"
    echo "  - Haven't forked the repo yet? Go to: https://github.com/livekit/sip and click Fork"
    echo "  - Using HTTPS? May need a personal access token"
    echo "  - Using SSH? Make sure your SSH key is added to GitHub"
    echo ""
    echo "For help, see: https://docs.github.com/en/get-started/quickstart/fork-a-repo"
fi
