# Github Stale PR Bot

This bot connects to the Github API and loads all Pull Requests it can find. It then iterates all PRs and checks whether they are assigned to someone or not. If a PR is not assigned and is older than 24 hours, a developer that is not the author of the PR is assigned automatically. If the PR is assigned to someone, it checks how old the PR is and if it is already too old (by default 3 days), it reminds that person on Slack to work on the PR.

## Crontab

An example crontab configuration could look like this:

```
0 7 * * * docker rm github-stale-pr-bot; docker run --name "github-stale-pr-bot" docker-registry.optiopay.com/github-stale-pr-bot -auth-key `cat ~/.githubbot-auth-key` -slack-url `cat ~/.githubbot-slack-url`
0 12 * * * docker rm github-stale-pr-bot; docker run --name "github-stale-pr-bot" docker-registry.optiopay.com/github-stale-pr-bot -auth-key `cat ~/.githubbot-auth-key`
0 16 * * * docker rm github-stale-pr-bot; docker run --name "github-stale-pr-bot" docker-registry.optiopay.com/github-stale-pr-bot -auth-key `cat ~/.githubbot-auth-key`
```

With this config a Slack reminder is only send in the morning. The bot tries to assign people two more times throughout the day.
