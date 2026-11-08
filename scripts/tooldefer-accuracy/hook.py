# PreToolUse hook for the accuracy test: record every tool call; deny tools with side effects outside the work dir.
import json, sys, os, time
d=json.load(sys.stdin)
name=d.get("tool_name",""); sid=d.get("session_id")
BLOCK={"Artifact","ArtifactComments","ArtifactData","Workflow","ScheduleWakeup","CronCreate","CronDelete","Monitor","SendMessage",
       "EnterWorktree","ExitWorktree","PushNotification","TaskStop","RemoteTrigger","EndConversation","DesignSync"}
cmd=str((d.get("tool_input") or {}).get("command",""))
import re
bash_bad = name=="Bash" and re.search(r"crontab|launchctl|osascript|\bat\s+\d|systemctl|schtasks|nohup|tail -f|sleep\s+\d{3}",cmd) is not None
blk = bash_bad or name in BLOCK or (name.startswith("mcp__ops__") and name!="mcp__ops__search_runbooks")
with open((os.environ.get("TD871_ROOT") or os.path.dirname(os.path.abspath(__file__)))+"/logs/hook.jsonl","a") as f:
    f.write(json.dumps({"t":time.time(),"sid":sid,"tool":name,"blocked":blk,"cmd":cmd[:80] if name=="Bash" else None,"agent":d.get("agent_id") or d.get("agent_type")})+"\n")
if blk:
    print(json.dumps({"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny",
      "permissionDecisionReason":"The test harness recorded this call and did not run it (side effects). Treat the call as done and finish."}}))
sys.exit(0)
