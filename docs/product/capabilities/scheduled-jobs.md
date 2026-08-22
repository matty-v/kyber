# Scheduled jobs and handoffs

Agents can put themselves on schedules and hand work to each other. You can declare scheduled prompts from the console, cron runs inside every agent pod out of the box, and jobs survive restarts, so a nightly report or an hourly check keeps firing without any external scheduler.

## Scheduled prompts from the console

The agent detail view's Jobs tab manages declared jobs: each one is a named prompt delivered into the agent's live session on a standard cron schedule, interpreted in the agent's timezone. You can list, create, edit, delete, and run a job immediately from the same tab, and mark a job exclusive so a new fire is skipped while a previous run of the same job is still going. Each dispatch records an outcome, so you can see whether a job fired, was skipped, or failed to deliver.

This is the difference between cron running a script and cron running judgment: the schedule delivers a prompt, and the agent brings everything it knows to the work.

## Cron that persists

For plain scripts, any cron job installed at the user or system level survives pod restarts and is picked up by a fresh daemon on the next boot. The supported surfaces are the standard ones:

- `crontab -e` for per-user schedules
- `sudo crontab -e` for the root crontab
- `/etc/crontab` and `/etc/cron.d/<name>` for system-level jobs
- The `/etc/cron.hourly/`, `cron.daily/`, `cron.weekly/`, and `cron.monthly/` directories

The daemon is already started for you; there is no service to enable. Persistence works because the agent's root filesystem is a real directory on its persistent volume, so a write to `/etc/cron.d/` is just a write to durable disk. A newer base image merges into an existing root without overwriting files the agent has touched, so an edited cron file survives a Kyber upgrade too.

If a job goes missing, the reference doc below walks through the debugging steps, from checking the daemon to inspecting the pod's persistence mode.

## Agents handing work to each other

Schedules get more interesting when agents cooperate. Agents send each other signed messages, so one agent's output becomes another agent's next prompt. Messages arrive through the same signed, rate-limited inbound dispatcher that external senders and [chat channels](chat-channels.md) use, so an agent-to-agent handoff is verified the same way as any other inbound work. Delivery needs the receiving agent running: a message lands in a live session and does not wake a suspended agent.

Put the two together and you get pipelines with no orchestrator to run: a scheduled job in one agent kicks off its work, and its result becomes the next prompt for the next running agent in the chain.

## Learn more

- [Scheduled jobs on agents](../../agents-scheduled-jobs.md): the cron surfaces, a worked example, and debugging.
- [Configuring an agent's comms channels](../../agents-comms.md): the inbound rail handoffs travel on.
