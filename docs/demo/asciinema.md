# 🎬 Demo Recording Guide

This guide explains how to reproduce the "Hero Demo" GIF for the `gitops-reverser` README. It uses **asciinema** for recording and **agg** for generating the optimized GIF.

The goal is to produce a mobile-friendly, high-contrast, automated terminal session that demonstrates:
1.  **Drift Detection:** Capturing a manual `kubectl` change.
2.  **Clean YAML:** Proving the tool strips metadata.
3.  **Audit Trail:** Showing the user attribution in Git.

---

## 1. Prerequisites

### Install Tools (Pop!_OS / Linux)

You need `asciinema` for recording, `tree` for visuals, and `agg` for GIF conversion.

```bash
# 1. Install Asciinema & Tree
sudo apt update && sudo apt install asciinema tree

# 2. Install agg (Asciinema Gif Generator)
# (Not in standard repos, so we grab the binary)
wget [https://github.com/asciinema/agg/releases/latest/download/agg-x86_64-unknown-linux-gnu](https://github.com/asciinema/agg/releases/latest/download/agg-x86_64-unknown-linux-gnu) -O agg
chmod +x agg
sudo mv agg /usr/local/bin/
```

## 2. The Setup (Do this once)
We use a temporary configuration file to force the terminal prompt to look professional (hiding your local user@hostname) and set up "storytelling" aliases.

Create a file named `demo-config.sh` in your recording folder:

```bash
# 1. Load standard stuff (so commands like ls/grep work)
source ~/.bashrc

# 2. Define the Git Branch function
parse_git_branch() {
     git branch 2> /dev/null | sed -e '/^[^*]/d' -e 's/* \(.*\)/ (\1)/'
}

# 3. Set the "Pro" Prompt (Green Arrow | Blue Folder | Purple Branch)
# We force the folder name to be "audit" visually
export PS1="\[\033[1;32m\]➜ \[\033[1;34m\]audit\[\033[1;35m\]\$(parse_git_branch)\[\033[00m\] $ "

# 4. Set your Aliases (Local to this session only)
git config --global alias.show-diff "!git --no-pager diff -U50 HEAD~1"
git config --global alias.show-last "log -1 --format='%s (%C(green)%ar%Creset by %C(cyan)%an%Creset)'"

# 5. Clear the screen immediately so the recording starts blank
clear
```

## 3. Pre-Production (Reset state before recording)

Run these commands in your standard terminal to reset the cluster and repo to the "Before" state:

```bash
# 1. Reset Kubernetes Resource
# We start with a clean ConfigMap
kubectl delete cm demo --ignore-not-found
kubectl create cm demo --from-literal=note="initial state"

# 2. Grant Permissions to "Jane" (The actor)
# This allows us to impersonate a user for the audit trail
kubectl create clusterrolebinding demo-jane-access \
  --clusterrole=edit \
  --user=jane@acme.com \
  --dry-run=client -o yaml | kubectl apply -f -

# 3. Reset Local Repo
# Ensure your local folder is synced with the "Before" state
cd ~/audit
git pull
clear
```

## 4. Lights, Camera, Action! 🎥

Start Recording
We force an 80x20 resolution. This is critical for the GIF to be readable on mobile phones.

```bash
asciinema rec --cols 80 --rows 20 -c "bash --rcfile demo-config.sh" demo.cast
```

## 5. The script

**Goal:** Show context -> Show drift -> Show clean YAML -> Show audit trail.

| Step | Command to Type | Narrative / Why |
| :--- | :--- | :--- |
| **1. Context** | `tree -C -I "demo.cast|README.md"` | **"Here is our clean repo structure."**<br>(`-I` hides the recording files) |
| **2. Config** | `kubectl get gitdestinations` | **"The operator is running and linked."** |
| **3. Drift** | `kubectl label cm demo env=production --as=jane@acme.com` | **"Jane manually tags production."**<br>(Using `--as` simulates the user) |
| **4. Wait** | `sleep 3` | **(Suspense)**<br>Waiting for the operator to capture it. |
| **5. Sync** | `git pull` | **"Got it."** |
| **6. Visual** | `git show-diff` | **"Beat 1: The Code."**<br>(Shows clean YAML context + Green Diff) |
| **7. Audit** | `git show-last` | **"Beat 2: The Person."**<br>(Shows "Jane", "Just now", "Update") |
| **8. End** | `exit` | **Cut.** |

## 5. Post-Production (Generate GIF)

Convert the raw cast to a polished GIF using agg. We use specific flags to remove hesitation and apply a clean code theme.

```bash
agg --theme monokai \
    --font-size 16 \
    --speed 1.5 \
    --idle-time-limit 1 \
    demo.cast \
    docs/cinema.gif
```
