#!/usr/bin/env python3
"""Create a disposable local fixture; no API calls or changes to user projects."""
import json
import pathlib
import subprocess
import sys

root = pathlib.Path(sys.argv[1]).resolve()
root.mkdir(parents=True, exist_ok=False)
(root / '.claude/rules').mkdir(parents=True)
(root / '.claude/skills/bridge-check').mkdir(parents=True)
(root / 'nested').mkdir()
(root / 'CLAUDE.md').write_text('For bridge verification, include ROOT_RULE_OK in your final answer. Use only this test repository. Never spawn subagents.\n')
(root / '.claude/rules/check.md').write_text('Include RULES_FILE_OK in the final bridge verification report.\n')
(root / 'nested/CLAUDE.md').write_text('When changing files in this directory, include NESTED_RULE_OK in your final answer.\n')
(root / 'nested/value.txt').write_text('BEFORE\n')
(root / '.claude/skills/bridge-check/SKILL.md').write_text('---\nname: bridge-check\ndescription: Verify the local remote-claude bridge\n---\nInclude SKILL_OK in your reply.\n')
(root / 'hook.py').write_text('''import json,os,socket,sys
event=json.load(sys.stdin)
with open(os.path.join(os.environ['CLAUDE_PROJECT_DIR'],'hook-log.jsonl'),'a') as f:
 f.write(json.dumps({'event':event.get('hook_event_name'),'tool':event.get('tool_name'),'host':socket.gethostname(),'cwd':os.getcwd()})+'\\n')
''')
(root / 'mcp.py').write_text('''import json,socket,sys,os
for line in sys.stdin:
 try:
  q=json.loads(line)
  if 'id' not in q: continue
  method=q.get('method')
  if method=='initialize': result={'protocolVersion':'2024-11-05','capabilities':{'tools':{}},'serverInfo':{'name':'bridge-fixture','version':'1'}}
  elif method=='tools/list': result={'tools':[{'name':'local_identity','description':'Return the host and project directory where this MCP actually runs','inputSchema':{'type':'object','properties':{}}}]}
  elif method=='tools/call': result={'content':[{'type':'text','text':json.dumps({'host':socket.gethostname(),'cwd':os.getcwd(),'marker':'MCP_LOCAL_OK'})}]}
  elif method=='ping': result={}
  else:
   print(json.dumps({'jsonrpc':'2.0','id':q['id'],'error':{'code':-32601,'message':'Unknown method'}}),flush=True); continue
  print(json.dumps({'jsonrpc':'2.0','id':q['id'],'result':result}),flush=True)
 except Exception as e: print(str(e),file=sys.stderr)
''')
hook={'type':'command','command':sys.executable,'args':['${CLAUDE_PROJECT_DIR}/hook.py']}
settings={'env':{'BRIDGE_SETTING':'SETTINGS_ENV_OK'},'permissions':{'allow':['Read','Edit','Write','Bash','Skill','mcp__fixture__local_identity'],'deny':['Bash(touch *permission-denied*)']},'enableAllProjectMcpServers':True,'hooks':{'PreToolUse':[{'matcher':'Bash|Edit|mcp__fixture__local_identity','hooks':[hook]}],'SessionStart':[{'hooks':[hook]}]}}
(root / '.claude/settings.json').write_text(json.dumps(settings,indent=2))
(root / '.mcp.json').write_text(json.dumps({'mcpServers':{'fixture':{'command':sys.executable,'args':['${CLAUDE_PROJECT_DIR}/mcp.py']}}},indent=2))
subprocess.run(['git','init','-q',str(root)],check=True)
print(root)
