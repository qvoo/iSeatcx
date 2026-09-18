<template>
  <div class="page">
    <div class="toolbar">
      <div class="logo"><img :src="logo" class="logo-img" alt="iSeat" /> 自习室自动占座系统</div>
      <div class="row" style="gap:10px;flex:none;width:auto">
        <span v-if="newTaskMsg" class="muted" style="color:#22a06b">{{ newTaskMsg }}</span>
        <button class="btn btn-ghost btn-sm" @click="refreshAll">刷新</button>
        <button class="btn btn-danger btn-sm" @click="logout">退出登录</button>
      </div>
    </div>

    <div class="grid">
      <!-- 功能1: 手动选座（含作用域） -->
      <div class="card">
        <h3><span class="icon" style="background:#3b7cff">1</span> 手动选择其他座位</h3>
        <div class="row" style="gap:8px;align-items:center">
          <span class="muted">作用域</span>
          <span class="pill" :class="{active: scope.mode==='single'}" @click="scope.mode='single';onScopeChange()">单账号</span>
          <span class="pill" :class="{active: scope.mode==='all'}" @click="scope.mode='all';onScopeChange()">全部账号·批量</span>
          <span class="grow"></span>
          <button class="btn btn-ghost btn-sm" @click="showAccPop=!showAccPop">管理账号</button>
        </div>
        <div v-if="scope.mode==='single'" class="label" style="margin-top:8px">
          作用于账号
          <select class="select" style="margin-top:4px" v-model="scope.accountId" @change="onScopeChange">
            <option v-for="a in accounts" :key="a.id" :value="a.id">{{ a.username }}</option>
          </select>
        </div>
        <div v-else class="muted" style="margin-top:8px">共 {{ accounts.length }} 个账号，将批量占座（座位自动分配）</div>

        <label class="label">自习室</label>
        <select class="select" v-model="manual.roomId" @change="onRoomChange">
          <option value="" disabled>选择自习室…</option>
          <option v-for="r in rooms" :key="r.id" :value="r.id">{{ r.name }}（{{ r.open_time || '--' }}~{{ r.cap_end || '--' }}）</option>
        </select>

        <div class="row" style="margin-top:12px;gap:8px">
          <span class="pill" :class="{active: manual.day==='today'}" @click="manual.day='today';loadSeats()">今天 {{ todayLabel }}</span>
          <span class="pill" :class="{active: manual.day==='tomorrow'}" @click="manual.day='tomorrow';loadSeats()">明天 {{ tomorrowLabel }}</span>
          <span class="muted grow" style="text-align:right">已选座位：<b style="color:#3b7cff">{{ manual.seatNum || '--' }}</b></span>
        </div>

        <div style="max-height:230px;overflow:auto;margin-top:10px;border:1px solid #eef1f7;border-radius:14px;padding:10px;background:#fafbfe">
          <div v-if="seatLoading" class="muted" style="text-align:center;padding:20px">座位加载中…</div>
          <div v-else class="seat-grid">
            <div v-for="s in manualSeats" :key="s.num"
                 class="seat" :class="{sel: s.num===manual.seatNum, off: !s.available}"
                 :title="s.available ? '可选' : (s.disabled ? '暂停预约' : '已被占用')"
                 @click="pickSeat(s)">{{ s.num }}</div>
          </div>
          <div class="muted" style="display:flex;gap:14px;justify-content:center;padding-top:8px">
            <span><i class="dot" style="background:#e7f1ff"></i> 可选</span>
            <span><i class="dot" style="background:#d5dae4"></i> 占用/暂停</span>
          </div>
          <div v-if="!seatOccKnown" class="muted" style="text-align:center;padding-top:6px;color:#c2410c">
            该校接口不返回座位占用情况，格子只用来选座位号
          </div>
        </div>

        <label class="label">或手动输入座位号</label>
        <div class="row" style="gap:8px;align-items:center">
          <input class="input" v-model="manual.seatInput" placeholder="如 117（网格未显示/加载失败时可直接输入）"
                 style="flex:1" @keyup.enter="applySeatInput" />
          <button class="btn btn-ghost btn-sm" style="flex:none" @click="applySeatInput">使用</button>
        </div>

        <label class="label">备选座位（点击选择，可多选）</label>
        <div style="max-height:180px;overflow:auto;border:1px solid #ffe2bd;border-radius:14px;padding:10px;background:#fffdf8">
          <div v-if="seatLoading" class="muted" style="text-align:center;padding:16px">座位加载中…</div>
          <div v-else-if="manualSeats.length === 0" class="muted" style="text-align:center;padding:16px">请先选择自习室</div>
          <div v-else class="seat-grid">
            <div v-for="s in manualSeats" :key="'alt'+s.num"
                 class="seat" :class="{alt: altSeatList.includes(s.num)}"
                 :title="'点击' + (altSeatList.includes(s.num) ? '取消' : '设为') + '备选座位'"
                 @click="toggleAltSeat(s.num)">{{ s.num }}</div>
          </div>
        </div>
        <div v-if="altSeatList.length" style="margin-top:8px">
          <span class="muted">已选 {{ altSeatList.length }} 个备选：</span>
          <span v-for="n in altSeatList" :key="'c'+n" class="alt-chip" @click="toggleAltSeat(n)"
                title="点击移除">{{ n }} ×</span>
        </div>
        <div class="row" style="gap:8px;align-items:center;margin-top:8px">
          <input class="input" v-model="manual.altInput" placeholder="网格里没有？手动加一个座位号"
                 style="flex:1" @keyup.enter="addAltSeat" />
          <button class="btn btn-ghost btn-sm" style="flex:none" @click="addAltSeat">添加备选</button>
        </div>
        <p class="muted" style="margin-top:6px">
          备选座位会<strong>接力</strong>主座位的时段：主座位约到哪，就从那里接着往后约，时间不断档。
        </p>

        <label class="label">预约模式</label>
        <div class="pills">
          <span class="pill" :class="{active: manual.mode==='today_once'}" @click="manual.mode='today_once'">预约今天</span>
          <span class="pill" :class="{active: manual.mode==='tomorrow_once'}" @click="manual.mode='tomorrow_once'">预约明天</span>
          <span class="pill" :class="{active: manual.mode==='both'}" @click="manual.mode='both'">两个都选·每天自动</span>
        </div>
        <button class="btn btn-primary" style="width:100%;margin-top:18px" :disabled="!manual.seatNum" @click="openConfirm('seat')">确认预约</button>
        <div class="msg" :class="msgManualOk ? 'ok' : 'err'">{{ msg.manual }}</div>
        <p class="muted" style="margin-top:8px">签到无需现场扫码，到签到时间系统自动完成。</p>
      </div>

      <!-- 功能2: 任务管理 -->
      <div class="card card-scroll">
        <h3><span class="icon" style="background:#8a6cf0">2</span> 任务管理</h3>
        <div class="card-body">
          <div v-if="scopedTasks.length === 0" class="muted">暂无占座任务{{ scope.mode==='single' ? '（' + currentName + '）' : '' }}</div>
          <div class="scroll-list" v-else>
            <div v-for="t in scopedTasks" :key="t.id" class="list-item">
              <span class="tag blue">座位{{ t.seat_num }}</span>
              <span v-if="t.type === 'manual'" class="tag orange">手动时间段</span>
              <span v-else-if="t.auto_renew" class="tag orange">提前预约</span>
              <span class="grow">
                <b>{{ roomsMap[t.room_id] || t.room_name || '房间 '+t.room_id }}</b> · <span class="muted">{{ modeText(t.mode) }}</span>
                <span v-if="segText(t.segments)" class="muted"> · {{ segText(t.segments) }}</span>
                <span v-if="t.username" class="muted"> | {{ t.username }}</span><br/>
                <span class="muted">{{ t.last_action }}</span>
                <span v-if="t.grab_at" class="muted" style="display:block;margin-top:2px">
                  ⚡ 抢座响应：<b :class="t.grab_ms < 3000 ? 'fast' : (t.grab_ms < 8000 ? 'mid' : 'slow')">{{ (t.grab_ms/1000).toFixed(1) }}s</b>
                  <span class="muted"> · 抢到于 {{ new Date(t.grab_at).toLocaleString('zh-CN') }}</span>
                </span>
              </span>
              <span class="tag" :class="t.status==='active' ? 'green' : 'gray'">{{ t.status === 'active' ? '运行中' : t.status === 'paused' ? '已暂停' : '已结束' }}</span>
              <button v-if="t.status==='active'" class="btn btn-ghost btn-sm" @click="taskAction(t,'pause')">暂停</button>
              <button v-else class="btn btn-ghost btn-sm" @click="taskAction(t,'resume')">恢复</button>
              <button class="btn btn-danger btn-sm" @click="taskAction(t,'remove')">删除</button>
            </div>
          </div>

          <div style="margin-top:14px;border-top:1px solid #f0f3f9;padding-top:12px">
            <h3 style="font-size:14px;margin-bottom:6px">当前预约（{{ scope.mode==='all' ? '全部账号' : currentName }}）</h3>
            <div v-if="curReserves.length === 0" class="muted">无进行中的预约</div>
            <div class="scroll-list" v-else>
              <div v-for="r in curReserves" :key="r.id" class="list-item">
                <span class="tag blue">座位{{ r.seatNum }}</span>
                <span class="grow">{{ new Date(r.startTime).toLocaleString('zh-CN') }} ~ {{ new Date(r.endTime).toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit'}) }} <span class="muted">· {{ r.secondLevelName }}-{{ r.thirdLevelName }}</span> <span class="tag" :class="r.status===1?'green':'orange'">{{ STATUS_TEXT[r.status]||r.status }}</span><span v-if="r.username" class="muted"> · {{ r.username }}</span></span>
                <button class="btn btn-danger btn-sm" @click="reserveAction(r, 'cancel')" :disabled="r.status===1||r.status===3||r.status===5">取消</button>
                <button class="btn btn-ghost btn-sm" @click="reserveAction(r, 'signback')" :disabled="!(r.status===1||r.status===3||r.status===5)">退座</button>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 功能3: 快速预约 -->
      <div class="card card-scroll">
        <h3><span class="icon" style="background:#e08f1f">3</span> 快速预约</h3>
        <p class="muted" style="margin-bottom:8px">展示{{ scope.mode==='all' ? '全部账号' : currentName }}预约过的桌子，一键续约。</p>
        <div class="card-body">
          <div v-if="nearReserves.length === 0" class="muted">暂无预约记录</div>
          <div class="scroll-list" v-else>
            <div v-for="r in nearReserves.slice(0, 30)" :key="r.id" class="list-item">
              <span class="tag blue">座位{{ r.seatNum }}</span>
              <span class="grow">{{ r.secondLevelName }}-{{ r.thirdLevelName }} {{ new Date(r.startTime).toLocaleDateString('zh-CN') }} <span class="tag green">{{ STATUS_TEXT[r.status] || r.status }}</span><span v-if="r.username" class="muted"> · {{ r.username }}</span></span>
              <button class="btn btn-ghost btn-sm" @click="openQuick(r)">快速预约</button>
            </div>
          </div>
        </div>
      </div>

      <!-- 功能4/5: 学校规则（各校规则不同，按账号设置） -->
      <div class="card card-scroll">
        <h3><span class="icon" style="background:#22a06b">4</span> 学校规则</h3>
        <p class="muted" style="margin-bottom:8px">各校规则不同：按账号设置<b>抢座时刻</b>、<b>放号方式</b>（前一天 / 当天早上）、<b>单段最大小时</b>或<b>一次约满整天</b>，以及该校的座位参数。</p>
        <div class="card-body">
          <div v-if="accounts.length === 0" class="muted">暂无账号</div>
          <div v-for="a in accounts" :key="'rule'+a.id" class="rule-row">
            <div class="row" style="gap:8px;align-items:center">
              <span class="tag blue">{{ a.username }}</span>
              <span class="tag gray">{{ a.school || '默认学校' }}</span>
              <span class="grow"></span>
              <button class="btn btn-ghost btn-sm" @click="detectRules(a)" :disabled="detecting===a.id">
                {{ detecting===a.id ? '识别中…' : '重新识别规则' }}
              </button>
              <button class="btn btn-primary btn-sm" @click="saveRules(a)">保存</button>
            </div>
            <div class="row" style="gap:8px;margin-top:6px;flex-wrap:wrap">
              <label class="rule-f"><span>抢座时刻</span>
                <input class="input input-sm" v-model="ruleEdit[a.id].open_time" placeholder="19:00" />
              </label>
              <label class="rule-f"><span>放号方式</span>
                <select class="select" style="min-width:170px" v-model="ruleEdit[a.id].window_mode">
                  <option value="prev">前一天放号（如 19:00 抢明天）</option>
                  <option value="same">当天早上放号（如 07:00 抢当天）</option>
                </select>
              </label>
            </div>
            <div class="row" style="gap:8px;margin-top:6px;flex-wrap:wrap;align-items:center">
              <label class="rule-f"><span>单段最大小时</span>
                <input class="input input-sm" type="number" min="1" max="24" v-model.number="ruleEdit[a.id].max_hours"
                       :disabled="ruleEdit[a.id].full_day" placeholder="4" />
              </label>
              <label class="auto-renew" style="flex:0 0 auto">
                <input type="checkbox" v-model="ruleEdit[a.id].full_day" />
                <span>一次性约满整天（自动找闭馆时间，不用分段）</span>
              </label>
            </div>
            <div class="row" style="gap:6px;margin-top:6px;flex-wrap:wrap">
              <label class="rule-f"><span>系统代际</span>
                <select class="select" style="min-width:120px" v-model="ruleEdit[a.id].api_style">
                  <option value="seatengine">seatengine（新版·用seatId）</option>
                  <option value="seat">seat（旧版·用mappId）</option>
                </select>
              </label>
              <input class="input input-sm" v-model="ruleEdit[a.id].mapp_id" placeholder="mapp_id（seat代际用，如19774897）" />
            </div>
            <div class="row" style="gap:6px;margin-top:6px;flex-wrap:wrap">
              <input class="input input-sm" v-model="ruleEdit[a.id].seat_id" placeholder="seat_id（如105）" />
              <input class="input input-sm" v-model="ruleEdit[a.id].dept_id_enc" placeholder="dept_id_enc" />
              <input class="input input-sm" v-model="ruleEdit[a.id].seat_id_enc" placeholder="seat_id_enc" />
              <input class="input input-sm" v-model="ruleEdit[a.id].captcha_id" placeholder="captcha_id" />
            </div>
            <input class="input input-sm" style="margin-top:6px" v-model="ruleEdit[a.id].hall_url"
                   placeholder="预约大厅链接：粘贴后点保存，自动识别系统代际/学校参数/放号方式（可留空）" />
          </div>
        </div>
      </div>

      <!-- 功能5: 手动选时间段 -->
      <div class="card card-scroll">
        <h3><span class="icon" style="background:#c2410c">5</span> 手动选时间段</h3>
        <p class="muted" style="margin-bottom:8px">
          自己指定要约的<b>时间段</b>（可以加多段，例如 08:00~12:00、14:00~18:00）。
          系统到放号时刻按这些段去约，<b>不自动续约、不接力</b>，约到就停；已经在约的段会自动跳过。
        </p>
        <div class="card-body">
          <div class="row" style="gap:8px;flex-wrap:wrap">
            <label class="rule-f"><span>账号</span>
              <select class="select" style="min-width:150px" v-model.number="segForm.accountId" @change="loadSegRooms">
                <option v-for="a in accounts" :key="a.id" :value="a.id">{{ a.username }}</option>
              </select>
            </label>
            <label class="rule-f" style="flex:1;min-width:220px"><span>自习室</span>
              <select class="select" v-model="segForm.roomId">
                <option value="" disabled>选择自习室…</option>
                <option v-for="r in segRooms" :key="r.id" :value="r.id">{{ r.name }}（{{ r.open_time || '--' }}~{{ r.cap_end || '--' }}）</option>
              </select>
            </label>
          </div>
          <div class="row" style="gap:8px;margin-top:8px;flex-wrap:wrap;align-items:center">
            <label class="rule-f"><span>座位号</span>
              <input class="input input-sm" style="width:92px" v-model="segForm.seatNum" placeholder="如 117" />
            </label>
            <button class="btn btn-ghost btn-sm" style="flex:none" @click="useManualSeat">用模块1选的座位（{{ manual.seatNum || '--' }}）</button>
          </div>
          <label class="label" style="margin-top:8px">预约日期</label>
          <div class="pills">
            <span class="pill" :class="{active: segForm.mode==='today_once'}" @click="segForm.mode='today_once'">今天</span>
            <span class="pill" :class="{active: segForm.mode==='tomorrow_once'}" @click="segForm.mode='tomorrow_once'">明天</span>
            <span class="pill" :class="{active: segForm.mode==='both'}" @click="segForm.mode='both'">每天都要这些段</span>
          </div>
          <label class="label" style="margin-top:8px">时间段（可多段，HH:MM）</label>
          <div v-for="(r, i) in segForm.rows" :key="'seg'+i" class="row" style="gap:8px;align-items:center;margin-top:4px">
            <input class="input input-sm" style="width:88px" v-model="r.start" placeholder="08:00" />
            <span class="muted">~</span>
            <input class="input input-sm" style="width:88px" v-model="r.end" placeholder="12:00" />
            <button class="btn btn-ghost btn-sm" :disabled="segForm.rows.length<=1" @click="removeSegRow(i)">删除</button>
          </div>
          <button class="btn btn-ghost btn-sm" style="margin-top:8px" @click="addSegRow">+ 添加一段</button>
          <button class="btn btn-primary" style="width:100%;margin-top:14px" :disabled="segBusy" @click="createSegTask">
            {{ segBusy ? '创建中…' : '创建手动时间段任务' }}
          </button>
          <div class="msg" :class="segOk ? 'ok' : 'err'">{{ segMsg }}</div>
        </div>
      </div>
    </div>

    <div class="footer">
      <p>免责声明：本系统仅用于本人账号的座位预约自动化操作，请在遵守所在学校座位预约规则的前提下使用；因使用本工具产生的违约、风控或其他后果由使用者自行承担。本项目为开源学习工具，代码仅供学习交流。</p>
      <p style="margin-top:4px">开源地址：<a href="https://github.com/qvoo/iSeatcx" target="_blank" rel="noopener">https://github.com/qvoo/iSeatcx</a> · 欢迎提交 Issue 反馈问题</p>
    </div>

    <!-- 账号管理浮层 -->
    <div v-if="showAccPop" class="acc-pop">
      <h3 style="font-size:15px;margin-bottom:12px">账号管理</h3>
      <div class="row" style="gap:8px">
        <input class="input" v-model="newAcc.username" placeholder="手机号/学号" style="flex:1" />
        <input class="input" type="password" v-model="newAcc.password" placeholder="密码" style="flex:1" />
      </div>
      <p class="muted" style="margin-top:6px;font-size:12px">学校参数（选填，留空用默认）：每个账号可绑定自己的学校，跨校自动用该校座位/房间。</p>
      <input class="input input-sm" style="margin-top:6px;width:100%" v-model="newAcc.hall_url"
             placeholder="预约大厅链接（推荐：粘贴后自动识别系统代际/学校参数/放号方式）" />
      <div class="row" style="gap:6px;margin-top:6px;flex-wrap:wrap">
        <input class="input input-sm" v-model="newAcc.seat_id" placeholder="seat_id 默认105" />
        <input class="input input-sm" v-model="newAcc.dept_id_enc" placeholder="dept_id_enc" />
        <input class="input input-sm" v-model="newAcc.seat_id_enc" placeholder="seat_id_enc" />
        <input class="input input-sm" v-model="newAcc.captcha_id" placeholder="captcha_id" />
      </div>
      <button class="btn btn-primary btn-sm" style="width:100%;margin-top:8px" @click="addAccount" :disabled="addingAcc">添加账号</button>
      <div class="row" style="gap:8px;margin-top:10px;flex-wrap:wrap">
        <label class="batch-check" style="cursor:pointer"><input type="checkbox" :checked="allChecked" @change="toggleAll" /> 全选</label>
        <span class="muted">已选 {{ selectedIds.size }} 个</span>
        <span class="grow"></span>
        <button class="btn btn-danger btn-sm" :disabled="selectedIds.size===0" @click="batchDeleteAccount">批量删除</button>
      </div>
      <div style="margin-top:8px;max-height:280px;overflow:auto">
        <div v-for="a in accounts" :key="a.id" class="list-item" style="padding:10px 0">
          <input type="checkbox" :checked="selectedIds.has(a.id)" @change="toggleSelect(a.id)" class="acc-cb" />
          <span class="tag blue">{{ a.username }}</span>
          <span class="tag" :class="a.id===currentUser?'green':'gray'">{{ a.id===currentUser?'我':'批量' }}</span>
          <span class="tag gray grow" style="flex:0 0 auto;max-width:130px;overflow:hidden;text-overflow:ellipsis">{{ a.school || '默认学校' }}</span>
          <button class="btn btn-ghost btn-sm" @click="toggleSchoolEdit(a)">学校</button>
          <button class="btn btn-danger btn-sm" @click="delAccount(a.id)">删除</button>
        </div>
        <template v-for="a in accounts" :key="'e'+a.id">
          <div v-if="schoolEdit[a.id]?.open" class="school-edit" style="margin-bottom:8px">
            <div class="row" style="gap:6px;flex-wrap:wrap">
              <input class="input input-sm" v-model="schoolEdit[a.id].seat_id" placeholder="seat_id" />
              <input class="input input-sm" v-model="schoolEdit[a.id].dept_id_enc" placeholder="dept_id_enc" />
              <input class="input input-sm" v-model="schoolEdit[a.id].seat_id_enc" placeholder="seat_id_enc" />
              <input class="input input-sm" v-model="schoolEdit[a.id].captcha_id" placeholder="captcha_id" />
            </div>
            <div class="row" style="gap:8px;margin-top:6px">
              <button class="btn btn-ghost btn-sm" @click="schoolEdit[a.id].open=false">取消</button>
              <button class="btn btn-primary btn-sm" @click="saveSchool(a)">保存学校</button>
            </div>
          </div>
        </template>
      </div>
      <p class="muted" style="margin-top:6px">作用域为"全部账号"时，三个模块批量给每个账号占座（座位自动分配）。不同学校账号请分到对应房间再批量。</p>
    </div>

    <!-- 确认弹窗 -->
    <div v-if="confirm.open" class="mask" @click.self="confirm.open=false">
      <div class="dialog">
        <h4>确认预约<span v-if="confirm.type==='quick'" class="muted" style="font-weight:400">（{{ accounts.find(a=>a.id===confirm.accountId)?.username || '该账号' }}）</span><span v-else-if="scope.mode==='all'" class="muted" style="font-weight:400">（全部账号·批量）</span><span v-else class="muted" style="font-weight:400">（{{ currentName }}）</span></h4>
        <div v-if="confirm.info" style="font-size:13.5px" class="muted">
          自习室：{{ confirm.info.roomName }}<br/>
          座位号：<b style="color:#3b7cff">{{ confirm.info.seatNum }}</b> · 闭馆：{{ confirm.info.capEnd || '--' }}
          <span v-if="scope.mode==='all' && confirm.type!=='quick'" style="display:block;margin-top:4px">批量模式：该座位给第 1 个账号，其余账号自动分配该房间空闲座位</span>
        </div>
        <label class="label">预约模式</label>
        <div class="pills">
          <span class="pill" :class="{active: confirm.mode==='today_once'}" @click="confirm.mode='today_once'">预约今天</span>
          <span class="pill" :class="{active: confirm.mode==='tomorrow_once'}" @click="confirm.mode='tomorrow_once'">预约明天</span>
          <span class="pill" :class="{active: confirm.mode==='both'}" @click="confirm.mode='both'">两个都选·每天自动</span>
        </div>
        <label class="label">开始时间</label>
        <div class="row" style="gap:8px;align-items:center">
          <input class="input input-sm" style="width:110px" v-model="confirm.startTime" placeholder="08:00" />
          <span class="muted" style="font-size:12px">
            到点自动签到；该账号学校若开启了「一次性约满整天」，就从这里一直约到闭馆（{{ confirm.info?.capEnd || '--' }}）
          </span>
        </div>
        <label class="auto-renew">
          <input type="checkbox" v-model="confirm.autoRenew" />
          <span>抢到后持续续约（续约段到时间自动签到，任务管理可见）</span>
        </label>
        <div class="btns">
          <button class="btn btn-ghost" @click="confirm.open=false">再想想</button>
          <button class="btn btn-primary" :disabled="confirm.submitting" @click="submitConfirm">确认预约</button>
        </div>
        <div v-if="confirm.error" class="msg err">{{ confirm.error }}</div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api, clearToken, STATUS_TEXT, type Room, type Task, type Reserve, type Account } from './api'
import logo from './assets/logo.png'

const emit = defineEmits(['logout'])
const currentUser = ref(0)
const accounts = ref<Account[]>([])
const scope = reactive({ mode: 'single' as 'single' | 'all', accountId: 0 })
const showAccPop = ref(false)
const newAcc = reactive({ username: '', password: '', seat_id: '', dept_id_enc: '', seat_id_enc: '', captcha_id: '', hall_url: '' })
const addingAcc = ref(false)
const detecting = ref(0)
const newTaskMsg = ref('')
const mySeatId = ref('105')
// 批量删除账号
const selectedIds = ref<Set<number>>(new Set())
const allChecked = computed(() => accounts.value.length > 0 && selectedIds.value.size === accounts.value.length)
function toggleSelect(id: number) {
  const s = new Set(selectedIds.value)
  if (s.has(id)) { s.delete(id) } else { s.add(id) }
  selectedIds.value = s
}
function toggleAll() {
  selectedIds.value = allChecked.value ? new Set() : new Set(accounts.value.map(a => a.id))
}
async function batchDeleteAccount() {
  const selfSel = [...selectedIds.value].some(id => id === currentUser.value)
  if (!window.confirm(`删除选中的 ${selectedIds.value.size} 个账号${selfSel ? '（含当前登录账号，删除后需重新登录）' : ''}及其所有任务？`)) return
  const ids = [...selectedIds.value]
  try {
    const res = await api<{ ok: boolean; deleted_self?: boolean }>('/accounts/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
    if (res.deleted_self) { logout(); return }
  } catch (e: any) {
    alert('批量删除失败: ' + e.message)
    await refreshAll()
    return
  }
  // 乐观移除
  const del = new Set(ids)
  accounts.value = accounts.value.filter(a => !del.has(a.id))
  tasks.value = tasks.value.filter(t => !del.has(t.user_id))
  selectedIds.value = new Set()
  if (del.has(scope.accountId)) scope.accountId = accounts.value[0]?.id || 0
  await refreshAll()
}

const rooms = ref<Room[]>([])
const nearReserves = ref<(Reserve & { username?: string })[]>([])
const curReserves = ref<(Reserve & { username?: string })[]>([])
const tasks = ref<Task[]>([])

const currentName = computed(() => {
  const a = accounts.value.find(x => x.id === scope.accountId)
  return a ? a.username : '当前账号'
})
const roomsMap = computed<Record<string, string>>(() => {
  const m: Record<string, string> = {}
  for (const r of rooms.value) m[r.id] = r.name
  return m
})
const scopedTasks = computed(() => {
  if (scope.mode === 'single') return tasks.value.filter(t => t.user_id === scope.accountId)
  return tasks.value
})

// 账号学校参数（管理浮层编辑用）
const schoolEdit = reactive<Record<number, { open: boolean; seat_id: string; dept_id_enc: string; seat_id_enc: string; captcha_id: string }>>({})
function toggleSchoolEdit(a: Account) {
  if (!schoolEdit[a.id]) schoolEdit[a.id] = { open: false, seat_id: '', dept_id_enc: '', seat_id_enc: '', captcha_id: '' }
  schoolEdit[a.id].open = !schoolEdit[a.id].open
  schoolEdit[a.id].seat_id = a.seat_id || ''
  schoolEdit[a.id].dept_id_enc = a.dept_id_enc || ''
  schoolEdit[a.id].seat_id_enc = a.seat_id_enc || ''
  schoolEdit[a.id].captcha_id = a.captcha_id || ''
}
function schoolSeatId(accountId: number): string {
  const a = accounts.value.find(x => x.id === accountId)
  return (a && a.seat_id) || mySeatId.value
}

// 功能4/5：学校规则（按账号）
const ruleEdit = reactive<Record<number, {
  open_time: string; max_hours: number; seat_id: string;
  dept_id_enc: string; seat_id_enc: string; captcha_id: string; hall_url: string;
  api_style: string; mapp_id: string; window_mode: string; full_day: boolean
}>>({})
function syncRuleEdit() {
  // 每次都从服务端最新数据同步，避免"管理账号"与"学校规则"两处编辑互相用旧值覆盖
  for (const a of accounts.value) {
    ruleEdit[a.id] = {
      open_time: a.open_time || '19:00',
      max_hours: a.max_hours || 4,
      seat_id: a.seat_id || '',
      dept_id_enc: a.dept_id_enc || '',
      seat_id_enc: a.seat_id_enc || '',
      captcha_id: a.captcha_id || '',
      hall_url: a.hall_url || '',
      api_style: a.api_style || 'seatengine',
      mapp_id: a.mapp_id || '',
      window_mode: a.window_mode === 'same' ? 'same' : 'prev',
      full_day: !!a.full_day
    }
  }
}
async function saveRules(a: Account) {
  const e = ruleEdit[a.id]
  if (!e) return
  if (!/^\d{1,2}:\d{2}$/.test(e.open_time || '')) { alert('抢座时刻格式应为 HH:MM，如 19:00'); return }
  if (!e.max_hours || e.max_hours < 1 || e.max_hours > 24) { alert('单段最大小时数应在 1~24 之间'); return }
  try {
    const r = await api<any>(`/accounts/${a.id}/school`, { method: 'PUT', body: JSON.stringify(e) })
    await refreshAll()
    // 抓包识别后服务端可能回填了参数，用返回值刷新该账号的编辑框
    syncRuleEdit()
    const acc = r?.account
    newTaskMsg.value = acc
      ? `已保存「${a.username}」：${acc.api_style === 'seat' ? 'seat(旧版)' : 'seatengine(新版)'} · ${acc.window_mode === 'same' ? '当天早上放号' : '前一天放号'}${acc.full_day ? ' · 整段约满' : ''}`
      : `已保存「${a.username}」的学校规则`
    setTimeout(() => (newTaskMsg.value = ''), 6000)
  } catch (err: any) { alert('保存失败: ' + err.message) }
}

async function detectRules(a: Account) {
  detecting.value = a.id
  try {
    const r = await api<{ rule: string }>(`/accounts/${a.id}/detect`, { method: 'POST' })
    await refreshAll()
    syncRuleEdit()
    newTaskMsg.value = `「${a.username}」识别结果：${r.rule}`
    setTimeout(() => (newTaskMsg.value = ''), 8000)
  } catch (err: any) {
    alert('识别失败: ' + err.message)
  } finally { detecting.value = 0 }
}

// 功能5：手动选时间段
const segForm = reactive({
  accountId: 0,
  roomId: '',
  seatNum: '',
  mode: 'today_once' as 'today_once' | 'tomorrow_once' | 'both',
  rows: [{ start: '08:00', end: '12:00' }] as { start: string; end: string }[]
})
const segRooms = ref<Room[]>([])
const segBusy = ref(false)
const segMsg = ref('')
const segOk = ref(true)

function addSegRow() {
  const last = segForm.rows[segForm.rows.length - 1]
  // 默认接在上一段后面，省得手输
  segForm.rows.push(last && /^\d{1,2}:\d{2}$/.test(last.end) ? { start: last.end, end: '' } : { start: '', end: '' })
}
function removeSegRow(i: number) {
  if (segForm.rows.length > 1) segForm.rows.splice(i, 1)
}
function useManualSeat() {
  if (!manual.seatNum) { segMsg.value = '模块1还没选座位'; segOk.value = false; return }
  segForm.seatNum = manual.seatNum
  if (manual.roomId) segForm.roomId = manual.roomId
  segMsg.value = `已套用模块1的座位 ${manual.seatNum}`; segOk.value = true
}
async function loadSegRooms() {
  segForm.roomId = ''
  segRooms.value = []
  if (!segForm.accountId) return
  try {
    const res = await api<{ rooms: Room[] }>(`/rooms?account_id=${segForm.accountId}`)
    segRooms.value = res.rooms
  } catch (e: any) { segMsg.value = '自习室加载失败: ' + e.message; segOk.value = false }
}
async function createSegTask() {
  segMsg.value = ''
  if (!segForm.roomId) { segMsg.value = '请选择自习室'; segOk.value = false; return }
  const seatNum = (segForm.seatNum || '').trim()
  if (!/^\d{1,4}$/.test(seatNum)) { segMsg.value = '座位号只能是数字（如 117）'; segOk.value = false; return }
  const segments = segForm.rows
    .filter(r => (r.start || '').trim() && (r.end || '').trim())
    .map(r => ({ start: r.start.trim(), end: r.end.trim() }))
  if (segments.length === 0) { segMsg.value = '请至少填写一个时间段'; segOk.value = false; return }
  for (const s of segments) {
    if (!/^\d{1,2}:\d{2}$/.test(s.start) || !/^\d{1,2}:\d{2}$/.test(s.end)) {
      segMsg.value = `时间段格式应为 HH:MM（${s.start} ~ ${s.end}）`; segOk.value = false; return
    }
  }
  segBusy.value = true
  try {
    const room = segRooms.value.find(r => r.id === segForm.roomId)
    const acc = accounts.value.find(a => a.id === segForm.accountId)
    const res = await api<{ task: Task }>('/tasks', {
      method: 'POST',
      body: JSON.stringify({
        type: 'manual', mode: segForm.mode,
        room_id: segForm.roomId, seat_id: acc?.seat_id || mySeatId.value,
        seat_num: seatNum, room_name: room?.name || '',
        segments, auto_renew: false, account_id: segForm.accountId
      })
    })
    segOk.value = true
    segMsg.value = `任务 #${res.task.id} 已创建：${segments.map(s => s.start + '~' + s.end).join('、')}`
    await refreshAll()
  } catch (e: any) { segOk.value = false; segMsg.value = '创建失败: ' + e.message }
  finally { segBusy.value = false }
}

const manual = reactive({ roomId: '', seatNum: '', seatInput: '', altInput: '', mode: 'today_once', day: 'today' })
// 备选座位（可多选，接力主座位之后的时段）
const altSeatList = ref<string[]>([])
function toggleAltSeat(num: string) {
  altSeatList.value = altSeatList.value.includes(num)
    ? altSeatList.value.filter(x => x !== num)
    : [...altSeatList.value, num]
}
function addAltSeat() {
  const raw = (manual.altInput || '').trim()
  if (!raw) { msgManualOk.value = false; msg.manual = '请输入备选座位号'; return }
  if (!/^\d{1,4}$/.test(raw)) { msgManualOk.value = false; msg.manual = '备选座位号只能是数字'; return }
  const n = raw.padStart(3, '0')
  if (!altSeatList.value.includes(n)) altSeatList.value = [...altSeatList.value, n]
  manual.altInput = ''
  msgManualOk.value = true
  msg.manual = `已添加备选座位 ${n}`
}
const manualSeats = ref<{ num: string; available: boolean }[]>([])
const seatOccKnown = ref(true)
const seatLoading = ref(false)
const msg = reactive({ manual: '' })
const msgManualOk = ref(true)
const todayLabel = ref(formatDateCN(new Date()))
const tomorrowLabel = ref(formatDateCN(new Date(Date.now() + 86400000)))

function formatDateCN(d: Date): string { return `${d.getMonth() + 1}月${d.getDate()}日` }
function dayStr(offset: number): string {
  const d = new Date(Date.now() + offset * 86400000)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

const confirm = reactive({
  open: false, type: '', mode: 'today_once', submitting: false, error: '', accountId: 0, autoRenew: true, startTime: '08:00',
  info: null as null | { roomId: string; seatId: string; seatNum: string; roomName: string; capEnd: string }
})

function modeText(m: string) {
  return { today_once: '预约今天', tomorrow_once: '预约明天', both: '每天自动占座', qr: '扫码·占座到闭馆', manual: '手动时间段' }[m] || m
}
// 手动时间段任务：把 segments JSON 显示成 "08:00~12:00、14:00~18:00"
function segText(json: string | undefined): string {
  if (!json) return ''
  try {
    const arr = JSON.parse(json) as { start: string; end: string }[]
    return arr.map(s => `${s.start}~${s.end}`).join('、')
  } catch { return '' }
}

async function refreshAll() {
  const roomsQ = scope.mode === 'single' && scope.accountId ? `?account_id=${scope.accountId}` : ''
  // 各请求独立处理：某一接口失败（如某账号会话过期）不应阻断账号/任务列表刷新
  const [rRes, accRes, tRes] = await Promise.allSettled([
    api<{ rooms: Room[] }>(`/rooms${roomsQ}`),
    api<{ accounts: Account[] }>('/accounts'),
    api<{ tasks: Task[] }>('/tasks')
  ])
  if (accRes.status === 'fulfilled') {
    accounts.value = accRes.value.accounts
    syncRuleEdit()
    const validIds = new Set(accounts.value.map(a => a.id))
    selectedIds.value = new Set([...selectedIds.value].filter(id => validIds.has(id)))
  }
  if (tRes.status === 'fulfilled') tasks.value = tRes.value.tasks
  if (rRes.status === 'fulfilled') rooms.value = rRes.value.rooms

  // 任一接口未登录 -> 退出
  for (const res of [rRes, accRes, tRes]) {
    if (res.status === 'rejected' && String(res.reason?.message || '').includes('未登录')) { logout(); return }
  }
  if (!scope.accountId) { currentUser.value = accounts.value[0]?.id || 0; scope.accountId = accounts.value[0]?.id || 0 }
  const cur = accounts.value.find(a => a.id === (scope.mode === 'single' ? scope.accountId : currentUser.value))
  if (cur && cur.seat_id) mySeatId.value = cur.seat_id
  await loadReserves()
  if (manual.roomId) loadSeats()
}

async function loadReserves() {
  try {
    if (scope.mode === 'single') {
      const m = await api<{ cur: Reserve[]; near: Reserve[] }>(`/my-reserves?account_id=${scope.accountId}`)
      curReserves.value = m.cur.map(x => ({ ...x, username: currentName.value, accountId: scope.accountId }))
      nearReserves.value = m.near.map(x => ({ ...x, username: currentName.value, accountId: scope.accountId }))
    } else {
      const curAcc: (Reserve & { username?: string; accountId?: number })[] = []
      const nearAcc: (Reserve & { username?: string; accountId?: number })[] = []
      for (const a of accounts.value) {
        try {
          const m = await api<{ cur: Reserve[]; near: Reserve[] }>(`/my-reserves?account_id=${a.id}`)
          m.cur.forEach(x => curAcc.push({ ...x, username: a.username, accountId: a.id }))
          m.near.forEach(x => nearAcc.push({ ...x, username: a.username, accountId: a.id }))
        } catch { /* 跳过 */ }
      }
      curReserves.value = curAcc
      nearReserves.value = nearAcc
    }
  } catch { /* ignore */ }
}

function onScopeChange() {
  // 切账号/切作用域后，原来的房间、座位、备选座位都不再属于当前学校，必须清掉，
  // 否则会把上一个学校的房间/座位号发给另一个学校（必然约不上或约错）。
  manual.roomId = ''
  manual.seatNum = ''
  manual.seatInput = ''
  manual.altInput = ''
  altSeatList.value = []
  manualSeats.value = []
  refreshAll()
}
function onRoomChange() { manual.seatNum = ''; altSeatList.value = []; loadSeats() }

async function loadSeats() {
  const room = rooms.value.find(r => r.id === manual.roomId)
  if (!room) { manualSeats.value = []; return }
  seatLoading.value = true
  try {
    const day = manual.day === 'tomorrow' ? dayStr(1) : dayStr(0)
    const q = new URLSearchParams({ day })
    if (scope.mode === 'single' && scope.accountId) q.set('account_id', String(scope.accountId))
    const res = await api<{ seats: { num: string; available: boolean }[]; occupied_known?: boolean }>(`/rooms/${manual.roomId}/seats?${q}`)
    manualSeats.value = res.seats
    seatOccKnown.value = res.occupied_known !== false
    msgManualOk.value = true
    msg.manual = seatOccKnown.value
      ? `${day} 共 ${res.seats.length} 个座位 · 可选 ${res.seats.filter(s => s.available).length} 个`
      : `${day} 共 ${res.seats.length} 个座位（该校接口不返回占用情况，格子只表示座位号，请以官方页面为准）`
  } catch (e: any) { msgManualOk.value = false; msg.manual = '座位加载失败: ' + e.message }
  finally { seatLoading.value = false }
}

function pickSeat(s: { num: string; available: boolean }) {
  if (!s.available) return
  manual.seatNum = manual.seatNum === s.num ? '' : s.num
  // 主座位不能同时当备选
  if (manual.seatNum) altSeatList.value = altSeatList.value.filter(x => x !== manual.seatNum)
}

// 手动输入座位号（网格未显示/加载失败时使用）
function applySeatInput() {
  const raw = (manual.seatInput || '').trim()
  if (!raw) { msgManualOk.value = false; msg.manual = '请输入座位号'; return }
  if (!/^\d{1,4}$/.test(raw)) { msgManualOk.value = false; msg.manual = '座位号只能是数字（如 117）'; return }
  // 统一成 3 位（与网格一致），后端也会再 pad
  manual.seatNum = raw.padStart(3, '0')
  altSeatList.value = altSeatList.value.filter(x => x !== manual.seatNum)
  msgManualOk.value = true
  msg.manual = `已选择座位 ${manual.seatNum}`
}

function openConfirm(type: string) {
  const room = rooms.value.find(r => r.id === manual.roomId)
  if (!room) { msgManualOk.value = false; msg.manual = '请先选择自习室'; return }
  if (!manual.seatNum) { msgManualOk.value = false; msg.manual = '请点击方块选择座位'; return }
  const seatId = schoolSeatId(scope.mode === 'all' ? currentUser.value : scope.accountId)
  confirm.info = { roomId: room.id, seatId, seatNum: manual.seatNum, roomName: room.name, capEnd: room.cap_end || '--' }
  confirm.mode = manual.mode
  confirm.type = type
  confirm.error = ''
  confirm.open = true
}

function openQuick(r: Reserve & { accountId?: number }) {
  confirm.mode = 'today_once'
  confirm.type = 'quick'
  const accountId = r.accountId || scope.accountId
  confirm.accountId = accountId
  // roomId 可能是数字，必须转字符串（后端 room_id 为 string）
  // 闭馆时间从该房间已有任务的 cap_end 里取（没有就显示 --）
  const capEnd = tasks.value.find(t => String(t.room_id) === String(r.roomId) && t.cap_end)?.cap_end || ''
  confirm.info = { roomId: String(r.roomId), seatId: schoolSeatId(accountId), seatNum: r.seatNum, roomName: r.secondLevelName + '-' + r.thirdLevelName, capEnd }
  confirm.error = ''
  confirm.open = true
}

async function submitConfirm() {
  confirm.submitting = true
  confirm.error = ''
  const startTime = (confirm.startTime || '').trim() || '08:00'
  if (!/^\d{1,2}:\d{2}$/.test(startTime)) { confirm.error = '开始时间格式应为 HH:MM，如 08:00'; confirm.submitting = false; return }
  const isQuick = confirm.type === 'quick'
  const all = scope.mode === 'all' && !isQuick // 快速预约始终只给归属账号预约
  try {
    const accountId = isQuick ? confirm.accountId : scope.accountId
    const seatId = isQuick || scope.mode === 'single' ? schoolSeatId(accountId) : schoolSeatId(currentUser.value)
    const base = {
      type: confirm.type, mode: confirm.mode,
      room_id: confirm.info!.roomId, seat_id: seatId, seat_num: confirm.info!.seatNum,
      room_name: confirm.info!.roomName, start_time: startTime, duration_minutes: 240,
      recur_daily: confirm.mode === 'both', auto_renew: confirm.autoRenew,
      alt_seats: confirm.type === 'quick' ? '' : altSeatList.value.join(',')
    }
    if (all) {
      const res = await api<{ created: Task[] }>('/batch-task', {
        method: 'POST',
        body: JSON.stringify({ room_id: base.room_id, seat_id: base.seat_id, mode: base.mode, start_time: base.start_time, room_name: base.room_name, seats: [base.seat_num], account_ids: [], auto_renew: base.auto_renew, account_id: currentUser.value })
      })
      newTaskMsg.value = `已为 ${res.created.length} 个账号创建任务`
    } else {
      const res = await api<{ task: Task }>('/tasks', { method: 'POST', body: JSON.stringify({ ...base, account_id: accountId }) })
      newTaskMsg.value = `任务 #${res.task.id} 已创建（${accounts.value.find(a => a.id === accountId)?.username || ''}）`
    }
    confirm.open = false
    await refreshAll()
    setTimeout(() => (newTaskMsg.value = ''), 5000)
  } catch (e: any) { confirm.error = e.message }
  finally { confirm.submitting = false }
}

async function addAccount() {
  if (!newAcc.username || !newAcc.password) { alert('请输入账号密码'); return }
  addingAcc.value = true
  try {
    const body = {
      username: newAcc.username, password: newAcc.password,
      seat_id: newAcc.seat_id || undefined,
      dept_id_enc: newAcc.dept_id_enc || undefined,
      seat_id_enc: newAcc.seat_id_enc || undefined,
      captcha_id: newAcc.captcha_id || undefined,
      hall_url: newAcc.hall_url || undefined
    }
    const res = await api<{ account: Account; msg?: string }>('/accounts', { method: 'POST', body: JSON.stringify(body) })
    if (res.msg) { newTaskMsg.value = res.msg; setTimeout(() => (newTaskMsg.value = ''), 6000) }
    if (!res.msg || res.msg.indexOf('已按大厅链接更新') >= 0) {
      newAcc.username = ''; newAcc.password = ''; newAcc.seat_id = ''
      newAcc.dept_id_enc = ''; newAcc.seat_id_enc = ''; newAcc.captcha_id = ''; newAcc.hall_url = ''
    }
    await refreshAll()
  } catch (e: any) { alert('添加失败: ' + e.message) } finally { addingAcc.value = false }
}

async function delAccount(id: number) {
  const isSelf = id === currentUser.value
  if (!window.confirm(`删除该账号${isSelf ? '（当前登录账号）' : ''}及其所有任务？删除后需重新登录。`)) return
  try {
    const res = await api<{ ok: boolean; deleted_self?: boolean }>(`/accounts/${id}`, { method: 'DELETE' })
    if (res.deleted_self) { logout(); return }
  } catch (e: any) {
    alert('删除失败: ' + e.message)
    await refreshAll()
    return
  }
  // 乐观移除，保证界面立即反馈
  accounts.value = accounts.value.filter(a => a.id !== id)
  tasks.value = tasks.value.filter(t => t.user_id !== id)
  selectedIds.value = new Set([...selectedIds.value].filter(x => x !== id))
  if (scope.accountId === id) scope.accountId = accounts.value[0]?.id || 0
  await refreshAll()
}

async function saveSchool(a: Account) {
  const e = schoolEdit[a.id]
  if (!e) return
  try {
    await api(`/accounts/${a.id}/school`, { method: 'PUT', body: JSON.stringify({ seat_id: e.seat_id, dept_id_enc: e.dept_id_enc, seat_id_enc: e.seat_id_enc, captcha_id: e.captcha_id }) })
    e.open = false
    await refreshAll()
  } catch (err: any) { alert('保存失败: ' + err.message) }
}

async function taskAction(t: Task, action: string) {
  await api(`/tasks/${t.id}/action`, { method: 'POST', body: JSON.stringify({ action }) })
  await refreshAll()
}

async function reserveAction(r: Reserve & { accountId?: number }, action: string) {
  try { await api('/reserve-action', { method: 'POST', body: JSON.stringify({ action, reserve_id: r.id, account_id: r.accountId || scope.accountId }) }) }
  catch (e) { alert((e as any).message) }
  await refreshAll()
}

function logout() { clearToken(); emit('logout') }

onMounted(async () => {
  try { const me = await api<{ user: { id: number } }>('/me'); currentUser.value = me.user?.id || 0; scope.accountId = currentUser.value } catch { /* ignore */ }
  await refreshAll()
})
</script>

<style scoped>
.page { padding: 22px; max-width: 1180px; margin: 0 auto; }
.logo-img { width: 38px; height: 38px; border-radius: 12px; object-fit: cover; }
</style>
