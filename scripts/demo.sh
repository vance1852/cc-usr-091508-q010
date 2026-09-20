#!/usr/bin/env bash
# 端到端演示:夜间异物报告 → 并案 → 封闭传播 → 清除 → 复查 → 扩界失效 → 双确认 → 反查
# 前置:服务已启动(默认 :8080),数据库已迁移并导入 seed/seed.sql
set -euo pipefail
B="${BASE_URL:-http://127.0.0.1:8080}/api"
J='Content-Type:application/json'
FIELD=(-H "X-Actor-Role: field_ops" -H "X-Actor-Name: field-wang")
OPS=(-H "X-Actor-Role: ops_control" -H "X-Actor-Name: ops-li")
TOWER=(-H "X-Actor-Role: tower" -H "X-Actor-Name: tower-desk")
SWA=(-H "X-Actor-Role: contractor" -H "X-Actor-Name: sweeper-A")

jqpy() { python3 -c "import json,sys;${1}"; }

echo "== 1. 夜间报告:RWY36L-S3 靠近交叉道口,影像特征 a1b2...18 =="
R=$(curl -s -X POST $B/reports "${FIELD[@]}" -H "X-Actor-Source: patrol-car-7" -H $J \
  -d '{"segment_id":"RWY36L-S3","image_signature":"a1b2c3d4e5f60718","reporter":"patrol-chen","source":"patrol","risk_level":"medium"}')
EV=$(echo "$R" | jqpy "print(json.load(sys.stdin)['event']['id'])")
echo "   事件 $EV"
echo "$R" | jqpy "d=json.load(sys.stdin);print('   封闭 v%d:'%d['closure']['version'], d['closure']['segments'])"

echo "== 2. 塔台共同结论(实时、带来源) =="
curl -s "${TOWER[@]}" $B/tower/runway-status | jqpy "
for s in json.load(sys.stdin)['segments']:
    if not s['available']: print('   封闭', s['segment_id'], '← v%d'%s['closure_version'], s['closure_id'][:8])"

echo "== 3. 分派清除(边界内 4 区段) + 承包商完成 =="
for seg in RWY36L-S2 RWY36L-S3 TWY-A1 RWY36L-S4; do
  curl -s -X POST $B/events/$EV/tasks "${OPS[@]}" -H $J -d "{\"segment_id\":\"$seg\",\"contractor\":\"sweeper-A\"}" > /dev/null
done
for tid in $(curl -s "${SWA[@]}" $B/contractor/tasks | jqpy "[print(t['id']) for t in json.load(sys.stdin)['tasks'] if t['status']=='assigned']" | tr -d '\r'); do
  curl -s -X POST $B/tasks/$tid/complete "${SWA[@]}" | jqpy "print('   完成', json.load(sys.stdin)['segment_id'])"
done

echo "== 4. 复查通过(针对 v1) =="
RV=$(curl -s -X POST $B/events/$EV/reviews "${FIELD[@]}" -H $J -d '{}' | jqpy "print(json.load(sys.stdin)['id'])")
curl -s -X POST $B/reviews/$RV/complete "${FIELD[@]}" -H $J -d '{"outcome":"passed"}' > /dev/null
echo "   passed"

echo "== 5. 新证据 RWY36L-S2 影像相近 → 并案扩界,旧复查立即失效 =="
curl -s -X POST $B/reports "${FIELD[@]}" -H $J \
  -d '{"segment_id":"RWY36L-S2","image_signature":"a1b2c3d4e5f60719","reporter":"cam-3","source":"camera"}' \
  | jqpy "d=json.load(sys.stdin);print('   扩界:',d['expanded'],'→ v%d'%d['closure']['version'],d['closure']['segments'])"
curl -s "${OPS[@]}" $B/events/$EV | jqpy "
d=json.load(sys.stdin)
print('   事件状态:', d['event']['status'])
for r in d['reviews']: print('   复查 v%d → %s'%(r['closure_version'],r['status']))"

echo "== 6. 清除新区段 S1 并重新复查 v2 =="
curl -s -X POST $B/events/$EV/tasks "${OPS[@]}" -H $J -d '{"segment_id":"RWY36L-S1","contractor":"sweeper-A"}' > /dev/null
TID=$(curl -s "${SWA[@]}" $B/contractor/tasks | jqpy "print([t['id'] for t in json.load(sys.stdin)['tasks'] if t['segment_id']=='RWY36L-S1'][0])")
curl -s -X POST $B/tasks/$TID/complete "${SWA[@]}" > /dev/null
RV2=$(curl -s -X POST $B/events/$EV/reviews "${FIELD[@]}" -H $J -d '{}' | jqpy "print(json.load(sys.stdin)['id'])")
curl -s -X POST $B/reviews/$RV2/complete "${FIELD[@]}" -H $J -d '{"outcome":"passed"}' > /dev/null
echo "   复查 v2 passed"

echo "== 7. 恢复运行:迟到回执被拒 → 双确认闭环 =="
CL=$(curl -s "${OPS[@]}" $B/events/$EV | jqpy "print([c['id'] for c in json.load(sys.stdin)['closures'] if c['status']=='active'][0])")
curl -s -X POST $B/closures/$CL/confirm "${FIELD[@]}" -H $J -d '{"action":"reopen","closure_version":1}' \
  | jqpy "print('   v1 迟到回执 →', json.load(sys.stdin)['error'])"
curl -s -X POST $B/closures/$CL/confirm "${FIELD[@]}" -H $J -d '{"action":"reopen","closure_version":2}' \
  | jqpy "print('   场务确认 → applied:', json.load(sys.stdin)['applied'])"
curl -s -X POST $B/closures/$CL/confirm "${OPS[@]}" -H $J -d '{"action":"reopen","closure_version":2}' \
  | jqpy "d=json.load(sys.stdin);print('   运行控制确认 → applied:', d['applied'], ' 事件:', d['event_status'])"

echo "== 8. 航班延误反查 =="
curl -s -X POST $B/flights/delays "${OPS[@]}" -H $J \
  -d "{\"flight_no\":\"CA1732\",\"event_id\":\"$EV\",\"reason\":\"跑道封闭等待\",\"passenger_info\":\"旅客214人\"}" > /dev/null
curl -s "${OPS[@]}" $B/flights/CA1732/impact | jqpy "
d=json.load(sys.stdin)
ev=d['events'][0]
print('   事件', ev['event']['id'][:8], ev['event']['status'])
print('   封闭传播:', ' → '.join('v%d(%s)'%(c['version'],c['kind']) for c in ev['closures']))
print('   通知送达:', {n['channel']: n['status'] for n in ev['notifications']})"
