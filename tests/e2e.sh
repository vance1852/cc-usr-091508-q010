#!/usr/bin/env bash
# 端到端场景：夜间跑道异物处置全链路
#   0 健康检查与角色
#   1 交叉道口报告 -> 按位置/影像关联 + 相邻传播封闭
#   2 塔台只读最终结论；承包商只见本区段任务
#   3 重复报告幂等；迟到/越版本任务回执被拒
#   4 新定位证据扩大边界 -> 旧复查立即失效、PENDING 提案作废
#   5 缩小/恢复运行需场务+运行控制双独立确认；塔台看不到 PENDING
#   6 通知重复/迟到回执不越过当前版本
#   7 航班延误反查时间线（异物/传播/清除/复查/通知送达）
#   8 并发上报同一异物只产生一个事件；服务重启不丢未闭环事件
set -uo pipefail
BASE=${BASE:-http://127.0.0.1:8080}
OPS='-H X-User-ID:U-OPS-01'; TWR='-H X-User-ID:U-TWR-01'; FLD='-H X-User-ID:U-FLD-01'
NORTH='-H X-User-ID:U-CTR-N1'; EAST='-H X-User-ID:U-CTR-E1'
J='-H Content-Type:application/json'
pass=0; fail=0
ok(){ if [ "$1" = "$2" ]; then echo "PASS: $3 (= $1)"; pass=$((pass+1)); else echo "FAIL: $3 [want $2 got $1]"; fail=$((fail+1)); fi; }
contains(){ if echo "$1" | grep -q "$2"; then echo "PASS: $3"; pass=$((pass+1)); else echo "FAIL: $3 (缺 $2)"; echo "$1" | head -c 600; echo; fail=$((fail+1)); fi; }
notcontains(){ if echo "$1" | grep -q "$2"; then echo "FAIL: $3 (不应含 $2)"; fail=$((fail+1)); else echo "PASS: $3"; pass=$((pass+1)); fi; }
code(){ curl -s -o /tmp/body.json -w '%{http_code}' "$@"; }

echo "== 0 健康检查/鉴权/RBAC =="
ok "$(code $BASE/health)" 200 "健康检查"
ok "$(code $BASE/api/v1/events $OPS)" 200 "运行控制可查事件"
ok "$(code $BASE/api/v1/tower/status -H X-User-ID:NOPE)" 401 "未知用户被拒"
ok "$(code $BASE/api/v1/tower/status $NORTH)" 403 "承包商无权读塔台结论"

echo "== 1 初始报告：36L-S3 交叉道口附近，影像指纹 a1b2c3 =="
R1=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-NIGHT-01","source":"PILOT","reporter":"MU5102机长","airport_id":"PEK",
 "segment_id":"RWY36L-S3","ref_segment_id":"RWY27-T2","location_qual":"NEAR_INTERSECTION",
 "photo_url":"http://cam/01.jpg","image_sig":"a1b2c3d4e5f60011"
}')
echo "$R1"
EVT=$(echo "$R1" | python3 -c 'import sys,json;print(json.load(sys.stdin)["event_id"])')
ok "$(echo "$R1" | python3 -c 'import sys,json;print(json.load(sys.stdin)["new_version"])')" 1 "首报版本 v1"
# NEAR_INTERSECTION -> radius>=1：S2,S3,S4 + T2(交叉),T1,T3
for seg in RWY36L-S2 RWY36L-S3 RWY36L-S4 RWY27-T1 RWY27-T2 RWY27-T3; do contains "$R1" "$seg" "封闭传播到 $seg"; done
notcontains "$R1" "RWY36L-S1" "S1 两跳之外不封闭"
notcontains "$R1" "RWY27-T4" "T4 两跳之外不封闭"
# 传播依据
contains "$R1" "INTERSECTION" "留痕跨跑道交叉道口传播"

echo "== 2 塔台最终结论 / 承包商范围 =="
TW=$(curl -s $BASE/api/v1/tower/status $TWR)
echo "$TW" | python3 -c 'import sys,json;d=json.load(sys.stdin);[print(x["segment_id"],x["state"],x.get("reasons")) for x in d["segments"]]'
closed_n=$(echo "$TW" | python3 -c 'import sys,json;print(sum(1 for x in json.load(sys.stdin)["segments"] if x["state"]=="CLOSED"))')
ok "$closed_n" 6 "塔台看到 6 个 CLOSED，其余 AVAILABLE"
notcontains "$TW" "PENDING" "塔台视图无中间态"
notcontains "$TW" "旅客\|passenger\|P-1001\|陈旅客" "塔台结论不含旅客信息"
TN=$(curl -s "$BASE/api/v1/tasks?scope=current" $NORTH)
echo "北承包商任务: $TN"
north_n=$(echo "$TN" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["tasks"]))')
ok "$north_n" 4 "北承包商只见 4 个分派区段(S2,S3,T1,T2)"
notcontains "$TN" "RWY36L-S4\|RWY27-T3" "北承包商看不到东片区区段"
TE=$(curl -s "$BASE/api/v1/tasks?scope=current" $EAST)
east_n=$(echo "$TE" | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["tasks"]))')
ok "$east_n" 2 "东承包商只见 2 个分派区段(S4,T3)"
ok "$(code $BASE/api/v1/flights/MU5102-20260919/trace $NORTH)" 403 "承包商不能反查航班（旅客隔离）"

echo "== 3 重复报告幂等；同影像+同封闭集内位置 -> 关联且不开新版本 =="
R1DUP=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-NIGHT-01","source":"PILOT","reporter":"MU5102机长","airport_id":"PEK",
 "segment_id":"RWY36L-S3","location_qual":"POINT","image_sig":"a1b2c3d4e5f60011"}')
ok "$(echo "$R1DUP" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["event_id"]== "'$EVT'" and d["new_version"]==1)')" "True" "重复 report_id 幂等返回首报结论（首报本身无上游关联）"
R2=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-NIGHT-02","source":"CCTV","reporter":"场监摄像头","airport_id":"PEK",
 "segment_id":"RWY36L-S4","location_qual":"POINT","image_sig":"a1b2c3d4e5f60011"}')
ok "$(echo "$R2" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["linked"] and d["new_version"]==1)')" "True" "同影像/封闭集内报告关联到 $EVT，不开新版本"
contains "$R2" "SIG_MATCH" "关联依据含影像匹配"

echo "== 3b 承包商越权操作对方任务被拒 =="
EAST_TASK=$(echo "$TE" | python3 -c 'import sys,json;print(json.load(sys.stdin)["tasks"][0]["id"])')
NORTH_TASK=$(echo "$TN" | python3 -c 'import sys,json;print(json.load(sys.stdin)["tasks"][0]["id"])')
ok "$(code $J $EAST -X POST $BASE/api/v1/tasks/$NORTH_TASK/accept -d '{}')" 403 "东承包商不能接收北承包商任务"

echo "== 4 场务复查 v1 CLEAR；随后远端新证据扩大边界 =="
RV=$(curl -s $J $FLD -X POST $BASE/api/v1/events/$EVT/reviews -d '{"review_id":"REV-01","result":"CLEAR","finding":"S3 表面无异物"}')
contains "$RV" '"valid_version":1' "复查绑定 v1"
# 新报告：同一物体影像出现在 T4（原封闭集外）UNCERTAIN -> 扩大
R3=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-NIGHT-03","source":"RUNWAY_INSPECTION","reporter":"道面巡查","airport_id":"PEK",
 "segment_id":"RWY27-T4","location_qual":"UNCERTAIN","image_sig":"a1b2c3d4e5f60011"}')
echo "扩边界结果: $R3"
ok "$(echo "$R3" | python3 -c 'import sys,json;print(json.load(sys.stdin)["new_version"])')" 2 "新定位证据开出 v2"
contains "$R3" "RWY27-T4" "v2 封闭集纳入 T4"
contains "$R3" "RWY36L-S2" "v2 保留原封闭区段（并集）"
# 复查 REV-01 现在应失效（时间线/复查判定）
echo "-- 尝试在 v2 未完成清除时申请恢复运行（应被拒）--"
ok "$(code $J $OPS -X POST $BASE/api/v1/events/$EVT/reopen-proposal -d '{"reason":"试一下"}')" 409 "清除未完成不能申请恢复"

echo "== 4b 旧版本任务的迟到回执不能越过当前版本 =="
LATE=$(curl -s $J $NORTH -X POST $BASE/api/v1/tasks/$NORTH_TASK/complete -d '{"note":"迟到完成"}')
echo "$LATE"
contains "$LATE" "STALE_VERSION" "v1 任务完成回执被判越版本"
# v2 新任务生成，且 v1 未开始任务被取消
T2ALL=$(curl -s "$BASE/api/v1/tasks" $OPS)
contains "$T2ALL" "CANCELLED_STALE" "v1 未开始任务已作废"
v2_n=$(echo "$T2ALL" | python3 -c '
import sys,json
ts=json.load(sys.stdin)["tasks"]
print(len([t for t in ts if t["version"]==2 and t["status"]=="ASSIGNED"]))')
ok "$v2_n" 7 "v2 为 7 个封闭区段生成新任务"

echo "== 5 完成 v2 全部清除 + CLEAR 复查，缩小封闭需双确认 =="
export BASE
python3 - <<'EOF'
import json,subprocess,os
B=os.environ.get('BASE','http://127.0.0.1:8080')+"/api/v1"
def call(uid,method,path,body='{}'):
    return subprocess.run(['curl','-s','-H',f'X-User-ID:{uid}','-H','Content-Type: application/json','-X',method,
                           B+path,'-d',body],capture_output=True,text=True).stdout
ts=json.loads(call('U-OPS-01','GET','/tasks?scope=current'))["tasks"]
NORTH={'RWY36L-S2','RWY36L-S3','RWY27-T1','RWY27-T2'}
# 交叉区段(S3,T2)主责 NORTH，其余按承包商
for t in ts:
    uid='U-CTR-N1' if t['segment_id'] in NORTH else 'U-CTR-E1'
    call(uid,'POST',f"/tasks/{t['id']}/accept")
for t in ts:
    uid='U-CTR-N1' if t['segment_id'] in NORTH else 'U-CTR-E1'
    call(uid,'POST',f"/tasks/{t['id']}/start")
for t in ts:
    uid='U-CTR-N1' if t['segment_id'] in NORTH else 'U-CTR-E1'
    r=call(uid,'POST',f"/tasks/{t['id']}/complete",'{"note":"已清除"}')
    assert '"status":"COMPLETED"' in r, r
print("v2 全部任务已完成")
EOF
RV2=$(curl -s $J $FLD -X POST $BASE/api/v1/events/$EVT/reviews -d '{"review_id":"REV-02","result":"CLEAR","finding":"全部封闭区段复查通过"}')
contains "$RV2" '"valid_version":2' "v2 CLEAR 复查"
# 同一运行控制员不能同时充当两方
PROP=$(curl -s $J $OPS -X POST $BASE/api/v1/events/$EVT/reopen-proposal -d '{"reason":"清除完成，申请恢复"}')
echo "恢复提案: $PROP"
contains "$PROP" '"state":"PENDING"' "恢复运行提案为 PENDING"
# 提案期间塔台仍看到 CLOSED
TW2=$(curl -s $BASE/api/v1/tower/status $TWR)
still=$(echo "$TW2" | python3 -c 'import sys,json;print(sum(1 for x in json.load(sys.stdin)["segments"] if x["state"]=="CLOSED"))')
ok "$still" 7 "PENDING 期间塔台仍见全部封闭（最终结论未变）"
# OPS 不能代场务确认
ok "$(code $J $OPS -X POST $BASE/api/v1/events/$EVT/approvals -d '{"party":"FIELD"}')" 403 "运行控制不能代场务确认"
# 场务确认第一方
AP1=$(curl -s $J $FLD -X POST $BASE/api/v1/events/$EVT/approvals -d '{}')
contains "$AP1" '"field_approved":true' "场务确认已记录"
contains "$AP1" '"finalized":false' "单方确认不生效"
# 重复场务确认幂等
AP1DUP=$(curl -s $J $FLD -X POST $BASE/api/v1/events/$EVT/approvals -d '{}')
contains "$AP1DUP" '"duplicate":true' "重复确认幂等"
# 运行控制第二方 -> 生效
AP2=$(curl -s $J $OPS -X POST $BASE/api/v1/events/$EVT/approvals -d '{}')
contains "$AP2" '"ops_approved":true' "运行控制确认已记录"
contains "$AP2" '"finalized":true' "双独立确认齐备，恢复生效"
# 塔台最终结论变为 AVAILABLE；事件闭环
TW3=$(curl -s $BASE/api/v1/tower/status $TWR)
avail=$(echo "$TW3" | python3 -c 'import sys,json;print(sum(1 for x in json.load(sys.stdin)["segments"] if x["state"]=="AVAILABLE"))')
ok "$avail" 9 "塔台看到全场恢复 AVAILABLE"
ED=$(curl -s $BASE/api/v1/events/$EVT $OPS)
contains "$ED" '"closed":true' "事件已闭环"

echo "== 5b 迟到的确认不能越过当前版本（事件已闭环后再确认）=="
LATEAP=$(curl -s $J $OPS -X POST $BASE/api/v1/events/$EVT/approvals -d '{}')
contains "$LATEAP" '"duplicate":true' "闭环后的确认不改变结论"

echo "== 6 通知回执：重复/迟到 =="
NTF=$(curl -s "$BASE/api/v1/events/$EVT/notifications" $OPS)
# 取 v3(恢复) 的塔台通知做 ACK
NID=$(echo "$NTF" | python3 -c '
import sys,json
ns=json.load(sys.stdin)["notifications"]
n=[x for x in ns if x["version"]==3 and x["channel"]=="TOWER"][0]
print(n["id"])')
A1=$(curl -s $J $TWR -X POST $BASE/api/v1/notifications/$NID/receipts -d '{"kind":"ACK"}')
contains "$A1" '"status":"ACKED"' "塔台确认恢复通知"
A2=$(curl -s $J $TWR -X POST $BASE/api/v1/notifications/$NID/receipts -d '{"kind":"ACK"}')
contains "$A2" '"duplicate":true' "重复回执不重复推进"
# 迟到回执：对 v1 旧通知再 ACK（事件已有更新版本）
OID=$(echo "$NTF" | python3 -c '
import sys,json
ns=json.load(sys.stdin)["notifications"]
print([x for x in ns if x["version"]==1 and x["channel"]=="TOWER"][0]["id"])')
LR=$(curl -s $J $TWR -X POST $BASE/api/v1/notifications/$OID/receipts -d '{"kind":"ACK"}')
contains "$LR" "LATE" "旧版本通知的迟到回执被挡在当前版本之外"

echo "== 7 航班延误反查 =="
ok "$(code $J $OPS -X POST $BASE/api/v1/flights/MU5102-20260919/impacts -d '{
 "impact_id":"IMP-01","event_id":"'$EVT'","impact_kind":"DELAY"}')" 200 "记录航班延误并锚定事件"
TRACE=$(curl -s $BASE/api/v1/flights/MU5102-20260919/trace $OPS)
echo "$TRACE" | python3 -m json.tool | head -80
for kw in "REPORT" "PROPAGATION" "INTERSECTION" "TASK_COMPLETE" "REVIEW" "NOTIFY" "VERSION" "delivered"; do
  contains "$TRACE" "$kw" "反查时间线包含 $kw"
done
# REV-01 应显示已被扩边界失效
contains "$TRACE" "失效" "时间线标注旧复查已失效"
notcontains "$TRACE" "P-1001\|陈旅客\|E12345678" "反查结果不泄露旅客信息"

echo "== 8 并发：两个相同影像报告同时提交，只允许一个事件 =="
curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{"report_id":"RPT-C1","source":"CCTV","reporter":"c1","airport_id":"PEK","segment_id":"RWY36L-S1","location_qual":"POINT","image_sig":"f00df00df00d0001"}' >/tmp/c1.json &
curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{"report_id":"RPT-C2","source":"CCTV","reporter":"c2","airport_id":"PEK","segment_id":"RWY36L-S1","location_qual":"POINT","image_sig":"f00df00df00d0001"}' >/tmp/c2.json &
wait
E1=$(python3 -c 'import json;print(json.load(open("/tmp/c1.json"))["event_id"])')
E2=$(python3 -c 'import json;print(json.load(open("/tmp/c2.json"))["event_id"])')
echo "并发事件: $E1 / $E2"
ok "$E1" "$E2" "并发同影像报告归并为同一事件"

echo "== 5c PENDING 提案被扩大风险边界作废：旧复查失效、提案 SUPERSEDED、迟到确认被拒 =="
RX=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-X-01","source":"CCTV","reporter":"x","airport_id":"PEK",
 "segment_id":"RWY36L-S1","location_qual":"POINT","image_sig":"cafebabecafebabe"}')
XEVT=$(echo "$RX" | python3 -c 'import sys,json;print(json.load(sys.stdin)["event_id"])')
XTASK=$(curl -s "$BASE/api/v1/tasks?scope=current" $NORTH | python3 -c '
import sys,json
for t in json.load(sys.stdin)["tasks"]:
    if t["event_id"]=="'$XEVT'": print(t["id"]); break')
curl -s $NORTH -X POST $BASE/api/v1/tasks/$XTASK/accept -H 'Content-Type: application/json' -d '{}' >/dev/null
curl -s $NORTH -X POST $BASE/api/v1/tasks/$XTASK/start -H 'Content-Type: application/json' -d '{}' >/dev/null
curl -s $NORTH -X POST $BASE/api/v1/tasks/$XTASK/complete -H 'Content-Type: application/json' -d '{"note":"清除"}' >/dev/null
curl -s $J $FLD -X POST $BASE/api/v1/events/$XEVT/reviews -d '{"review_id":"REV-X1","result":"CLEAR"}' >/dev/null
PX=$(curl -s $J $OPS -X POST $BASE/api/v1/events/$XEVT/reopen-proposal -d '{"reason":"申请恢复"}')
contains "$PX" '"state":"PENDING"' "第二事件恢复提案 PENDING"
# 新证据：同一物体出现在 S3，封闭扩大，PENDING 应作废
RX2=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-X-02","source":"PILOT","reporter":"x2","airport_id":"PEK",
 "segment_id":"RWY36L-S3","location_qual":"POINT","image_sig":"cafebabecafebabe"}')
ok "$(echo "$RX2" | python3 -c 'import sys,json;print(json.load(sys.stdin)["new_version"])')" 3 "扩边界开出 v3"
XED=$(curl -s $BASE/api/v1/events/$XEVT $OPS)
echo "$XED" | python3 -c '
import sys,json
d=json.load(sys.stdin)
v2=[v for v in d["versions"] if v["version"]==2][0]
v3=[v for v in d["versions"] if v["version"]==3][0]
assert v2["decision_state"]=="SUPERSEDED", v2
assert v3["decision_state"]=="FINAL" and v3["decision"]=="CLOSED", v3
print("v2 SUPERSEDED / v3 CLOSED FINAL")'
ok "$?" 0 "PENDING 提案被扩边界证据作废，新版本直接 FINAL CLOSED"
# 场务对 v2 的确认现在迟到（显式指向 v2）
LATEAPX=$(curl -s $J $FLD -X POST $BASE/api/v1/events/$XEVT/approvals -d '{"target_version":2}')
contains "$LATEAPX" "LATE_VERSION" "针对已越过版本的确认被拒绝"
# 塔台仍只看到 CLOSED 结论，从未暴露 PENDING
TWO=$(curl -s "$BASE/api/v1/tower/status" $TWR)
notcontains "$TWO" "PENDING" "塔台全程不见 PENDING"
# 收尾闭环 X 事件（完成 v3 新任务 -> v3 CLEAR 复查 -> 提案 -> 双确认），避免污染后续事件的塔台视图
XEVT=$XEVT python3 - <<'EOF'
import json,subprocess,os
B=os.environ['BASE']+"/api/v1"; ev=os.environ['XEVT']
def call(uid,method,path,body='{}'):
    return subprocess.run(['curl','-s','-H',f'X-User-ID:{uid}','-H','Content-Type: application/json','-X',method,B+path,'-d',body],capture_output=True,text=True).stdout
ts=[t for t in json.loads(call('U-OPS-01','GET','/tasks?scope=current'))["tasks"]
    if t['event_id']==ev and t['status']=='ASSIGNED']
for t in ts:
    u='U-CTR-N1' if t['segment_id'] in ('RWY36L-S1','RWY36L-S3') else 'U-CTR-E1'
    call(u,'POST',f"/tasks/{t['id']}/accept"); call(u,'POST',f"/tasks/{t['id']}/start")
    assert '"status":"COMPLETED"' in call(u,'POST',f"/tasks/{t['id']}/complete",'{"note":"ok"}'), t
call('U-FLD-01','POST',f"/events/{ev}/reviews",'{"review_id":"REV-X3","result":"CLEAR"}')
call('U-OPS-01','POST',f"/events/{ev}/reopen-proposal",'{"reason":"恢复"}')
call('U-FLD-01','POST',f"/events/{ev}/approvals",'{}')
r=json.loads(call('U-OPS-01','POST',f"/events/{ev}/approvals",'{}'))
assert r['finalized'], r
print("X 事件闭环")
EOF

echo "== 5d 缩小封闭 -> 局部保持 -> 曾恢复区段重新封闭：任务必须重新分派 =="
RY=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-Y-01","source":"CCTV","reporter":"y","airport_id":"PEK",
 "segment_id":"RWY36L-S3","ref_segment_id":"RWY27-T2","location_qual":"NEAR_INTERSECTION",
 "image_sig":"0f0f0f0f0f0f0f0f"}')
YEVT=$(echo "$RY" | python3 -c 'import sys,json;print(json.load(sys.stdin)["event_id"])')
# 完成 v1 全部任务 + CLEAR 复查
export BASE
YEVT=$YEVT python3 - <<'EOF'
import json,subprocess,os
B=os.environ['BASE']+"/api/v1"; ev=os.environ['YEVT']
def call(uid,method,path,body='{}'):
    return subprocess.run(['curl','-s','-H',f'X-User-ID:{uid}','-H','Content-Type: application/json','-X',method,B+path,'-d',body],capture_output=True,text=True).stdout
NORTH={'RWY36L-S2','RWY36L-S3','RWY27-T1','RWY27-T2'}
ts=[t for t in json.loads(call('U-OPS-01','GET','/tasks?scope=current'))["tasks"] if t['event_id']==ev]
for t in ts:
    u='U-CTR-N1' if t['segment_id'] in NORTH else 'U-CTR-E1'
    call(u,'POST',f"/tasks/{t['id']}/accept"); call(u,'POST',f"/tasks/{t['id']}/start")
    assert '"status":"COMPLETED"' in call(u,'POST',f"/tasks/{t['id']}/complete",'{"note":"ok"}')
call('U-FLD-01','POST',f"/events/{ev}/reviews",'{"review_id":"REV-Y1","result":"CLEAR"}')
print("v1 tasks done + clear")
EOF
# 缩小：仅保留 S3 封闭
PY=$(curl -s $J $OPS -X POST $BASE/api/v1/events/$YEVT/reopen-proposal -d '{"reason":"仅 S3 复核中","keep_closed_segments":["RWY36L-S3"]}')
contains "$PY" '"decision":"RESTRICTED"' "缩小封闭提案为 RESTRICTED"
contains "$PY" '"state":"PENDING"' "缩小提案 PENDING"
curl -s $J $FLD -X POST $BASE/api/v1/events/$YEVT/approvals -d '{}' >/dev/null
curl -s $J $OPS -X POST $BASE/api/v1/events/$YEVT/approvals -d '{}' >/dev/null
TWY=$(curl -s "$BASE/api/v1/tower/status" $TWR)
echo "$TWY" | python3 -c '
import sys,json
d={x["segment_id"]:x["state"] for x in json.load(sys.stdin)["segments"]}
assert d["RWY36L-S3"]=="RESTRICTED", d["RWY36L-S3"]
for s in ["RWY36L-S2","RWY36L-S4","RWY27-T1","RWY27-T2","RWY27-T3"]: assert d[s]=="AVAILABLE", (s,d[s])
print("缩小后仅 S3 RESTRICTED，其余 AVAILABLE")'
ok "$?" 0 "缩小封闭后塔台结论正确（S3 限制使用）"
# 新证据：同一物体在 T4 出现（T2/T3 曾恢复），重新扩大
RY2=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-Y-02","source":"PILOT","reporter":"y2","airport_id":"PEK",
 "segment_id":"RWY27-T4","location_qual":"UNCERTAIN","image_sig":"0f0f0f0f0f0f0f0f"}')
ok "$(echo "$RY2" | python3 -c 'import sys,json;print(json.load(sys.stdin)["new_version"])')" 3 "重新封闭开出 v3"
# v3：T2/T3/T4 必须是新 ASSIGNED 任务；S3 持续封闭保持 COMPLETED
export PGBIN=/home/node/local/usr/lib/postgresql/15/bin; export LD_LIBRARY_PATH=/home/node/local/usr/lib/x86_64-linux-gnu; export PATH=$PGBIN:$PATH
psql -h /tmp/pgsock -p 55432 -U node -d runway -tA -c "
SELECT 'fresh_assigned='||count(*) FROM cleanup_tasks
 WHERE event_id='$YEVT' AND version=3 AND status='ASSIGNED' AND segment_id IN ('RWY27-T2','RWY27-T3','RWY27-T4');
SELECT 's3_completed='||count(*) FROM cleanup_tasks
 WHERE event_id='$YEVT' AND version=3 AND status='COMPLETED' AND segment_id='RWY36L-S3';"
fresh=$(psql -h /tmp/pgsock -p 55432 -U node -d runway -tAc "SELECT count(*) FROM cleanup_tasks WHERE event_id='$YEVT' AND version=3 AND status='ASSIGNED' AND segment_id IN ('RWY27-T2','RWY27-T3','RWY27-T4')")
s3c=$(psql -h /tmp/pgsock -p 55432 -U node -d runway -tAc "SELECT count(*) FROM cleanup_tasks WHERE event_id='$YEVT' AND version=3 AND status='COMPLETED' AND segment_id='RWY36L-S3'")
ok "$fresh" 3 "曾恢复的 T2/T3/T4 重新封闭时派全新任务"
ok "$s3c" 1 "持续封闭的 S3 保持已完成，不重复派单"
# 未完成新任务不能恢复
ok "$(code $J $OPS -X POST $BASE/api/v1/events/$YEVT/reopen-proposal -d '{}')" 409 "重新封闭后未清除不能恢复"
# 收尾：完成新任务 + 复查 + 双确认，闭环该事件
YEVT=$YEVT python3 - <<'EOF'
import json,subprocess,os
B=os.environ['BASE']+"/api/v1"; ev=os.environ['YEVT']
def call(uid,method,path,body='{}'):
    return subprocess.run(['curl','-s','-H',f'X-User-ID:{uid}','-H','Content-Type: application/json','-X',method,B+path,'-d',body],capture_output=True,text=True).stdout
NORTH={'RWY36L-T2'}
ts=[t for t in json.loads(call('U-OPS-01','GET','/tasks?scope=current'))["tasks"] if t['event_id']==ev and t['status']=='ASSIGNED']
for t in ts:
    u='U-CTR-N1' if t['segment_id']=='RWY27-T2' else 'U-CTR-E1'
    call(u,'POST',f"/tasks/{t['id']}/accept"); call(u,'POST',f"/tasks/{t['id']}/start")
    assert '"status":"COMPLETED"' in call(u,'POST',f"/tasks/{t['id']}/complete",'{"note":"ok"}')
call('U-FLD-01','POST',f"/events/{ev}/reviews",'{"review_id":"REV-Y3","result":"CLEAR"}')
call('U-OPS-01','POST',f"/events/{ev}/reopen-proposal",'{"reason":"恢复"}')
r1=json.loads(call('U-FLD-01','POST',f"/events/{ev}/approvals",'{}'))
r2=json.loads(call('U-OPS-01','POST',f"/events/{ev}/approvals",'{}'))
assert r2['finalized'], r2
print("Y 事件闭环")
EOF

echo "== 9 服务重启：未闭环事件不能消失，已闭环历史仍在 =="
RR=$(curl -s $J $OPS -X POST $BASE/api/v1/reports -d '{
 "report_id":"RPT-RESTART-01","source":"CCTV","reporter":"重启前报告","airport_id":"PEK",
 "segment_id":"RWY36L-S5","location_qual":"POINT","image_sig":"abcd1234efgh5678"}')
REVT=$(echo "$RR" | python3 -c 'import sys,json;print(json.load(sys.stdin)["event_id"])')
before_n=$(curl -s "$BASE/api/v1/events?airport_id=PEK" $OPS | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["events"]))')
pkill -f /tmp/runwayfod 2>/dev/null || true
for p in /proc/[0-9]*; do
  if grep -qa '/tmp/runwayfod' "$p/maps" 2>/dev/null; then kill "$(basename "$p")" 2>/dev/null || true; fi
done
sleep 1
DATABASE_URL="postgres://node@127.0.0.1:55432/runway?sslmode=disable" \
  SCHEMA_PATH="${SCHEMA_PATH:-$(cd "$(dirname "$0")/.." && pwd)/internal/store/schema.sql}" RUN_MIGRATIONS=0 \
  nohup /tmp/runwayfod >/tmp/runwayfod2.log 2>&1 &
sleep 1.5
after_open=$(curl -s "$BASE/api/v1/events?airport_id=PEK" $OPS | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["events"]))')
ok "$after_open" "$before_n" "重启后未闭环事件数量不变 ($after_open)"
ok "$(code $BASE/api/v1/events/$REVT $OPS)" 200 "重启后未闭环事件仍可取回"
ok "$(code $BASE/api/v1/events/$EVT $OPS)" 200 "重启后已闭环事件历史仍可查"

echo
echo "结果：PASS=$pass FAIL=$fail"
[ "$fail" -eq 0 ]
