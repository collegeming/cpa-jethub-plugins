package reg

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// maxPhoneDraws bounds how many SMS-pool numbers one job may burn when
// already-registered numbers keep failing the shared login claim path.
const maxPhoneDraws = 5

// regJob is one registration run: progress lines accumulate for the page,
// and the terminal state (success or error plus the artifacts produced) is
// kept for display after the run finishes.
type regJob struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	lines     []string
	phone     string
	fileName  string
	akPrefix  string
	errText   string
	done      bool
	succeeded bool
}

var job = &regJob{}

// log appends one timestamped progress line.
func (j *regJob) log(message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, time.Now().Format("15:04:05")+" "+message)
	if len(j.lines) > 200 {
		j.lines = j.lines[len(j.lines)-200:]
	}
}

// snapshot copies the display state out under the lock.
func (j *regJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	lines := make([]string, len(j.lines))
	copy(lines, j.lines)
	return map[string]any{
		"running":   j.running,
		"done":      j.done,
		"succeeded": j.succeeded,
		"phone":     j.phone,
		"file":      j.fileName,
		"ak_prefix": j.akPrefix,
		"error":     j.errText,
		"started":   j.startedAt.Format("2006-01-02 15:04:05"),
		"lines":     lines,
	}
}

// start launches a registration run unless one is already in flight or the
// plugin's write switch is off.
func (j *regJob) start() error {
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return fmt.Errorf("已有注册任务进行中")
	}
	if !settings().Enabled {
		j.mu.Unlock()
		return fmt.Errorf("插件已关闭（enabled=false），不发起注册")
	}
	j.running = true
	j.done = false
	j.succeeded = false
	j.lines = nil
	j.phone = ""
	j.fileName = ""
	j.akPrefix = ""
	j.errText = ""
	j.startedAt = time.Now()
	j.mu.Unlock()

	go j.run()
	return nil
}

// finish records the terminal state.
func (j *regJob) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	j.done = true
	j.succeeded = err == nil
	if err != nil {
		j.errText = err.Error()
	}
}

// run is the registration orchestration: draw a number, register it (or claim
// an already-registered one via the shared registerui login ticket), mint the
// OAuth credential, then persist it through the host so the codearts provider
// picks it up immediately.
func (j *regJob) run() {
	cfg := settings()
	log := j.log
	host := &abiboot.Host{}
	transport := transportFor(host)
	d := transport.do

	fail := func(stage string, err error) {
		log(stage + ": " + err.Error())
		j.finish(fmt.Errorf("%s: %w", stage, err))
	}

	pool := newSMSPool(d, cfg)
	if amount, errAmount := pool.leftAmount(); errAmount == nil {
		log("接码余额: " + amount)
	}

	var lastErr error
	for attempt := 1; attempt <= maxPhoneDraws; attempt++ {
		log(fmt.Sprintf("正在取号（第 %d/%d 次）...", attempt, maxPhoneDraws))
		phone, errPhone := pool.getPhone()
		if errPhone != nil {
			fail("取号失败", errPhone)
			return
		}
		j.mu.Lock()
		j.phone = phone
		j.mu.Unlock()
		log("取到号码: " + phone)

		info, errAttempt := j.attemptPhone(pool, transport, phone, cfg, log)
		if errAttempt != nil {
			lastErr = errAttempt
			log("本号未成功: " + errAttempt.Error() + "，释放后重试")
			pool.release(phone)
			continue
		}

		log("账号就绪: " + info.DomainName)
		credential, errCred := obtainCredential(transport, info, host, cfg, log)
		if errCred != nil {
			fail("取凭证失败", errCred)
			return
		}

		fileName := cfg.NamePrefix + "-" + phone + ".json"
		raw, errMarshal := json.Marshal(credential)
		if errMarshal != nil {
			fail("凭证编码失败", errMarshal)
			return
		}
		if _, errSave := host.SaveAuth(fileName, raw); errSave != nil {
			fail("保存凭证失败", errSave)
			return
		}

		j.mu.Lock()
		j.fileName = fileName
		if len(credential.AccessKeyID) > 8 {
			j.akPrefix = credential.AccessKeyID[:8] + "…"
		} else {
			j.akPrefix = credential.AccessKeyID
		}
		j.mu.Unlock()
		log("凭证已写入 " + fileName + "，账号池即刻可用")
		j.finish(nil)
		return
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("已用尽 %d 次取号仍未成功", maxPhoneDraws)
	}
	fail("注册任务失败", lastErr)
}

// attemptPhone runs check_identity then either the fresh-register path or the
// shared quick_login claim path. On soft failures (already registered but
// login impossible) it returns an error so the caller can release and redraw.
func (j *regJob) attemptPhone(pool *smsPool, transport sessionTransport, phone string, cfg Config, log func(string)) (*accountInfo, error) {
	d := transport.do
	reg := &huaweiRegister{doer: d}

	identity, errIdentity := reg.checkIdentity(phone)
	if errIdentity != nil {
		return nil, fmt.Errorf("校验手机号失败: %w", errIdentity)
	}

	oldMsg, oldCode := pool.snapshotMsg(phone)
	sessionID := reg.geetestInit()
	if sessionID == "" {
		log("验证码会话初始化失败，自动跳过验证")
	} else if errRisk := reg.checkIdentityRisk(sessionID, phone); errRisk != nil {
		log("风控校验未通过: " + errRisk.Error())
	}

	if identity.IdentityUsed {
		log("手机号已注册，走共用 registerui 登录取 ticket（quick_login）")
		return j.claimExisting(pool, reg, phone, sessionID, oldMsg, oldCode, cfg, log)
	}
	if !identity.CanUse {
		return nil, fmt.Errorf("手机号不可用: %s", truncate(identity.Reason, 120))
	}

	log("手机号可用，发送验证码...")
	if errSend := reg.sendVerifyCode(sessionID, phone); errSend != nil {
		return nil, fmt.Errorf("发送验证码失败: %w", errSend)
	}
	log("验证码已发送，等待短信...")
	code, _, errSMS := pool.waitSMS(phone, cfg.SMSWaitMS, oldMsg, oldCode, log)
	if errSMS != nil {
		return nil, fmt.Errorf("等待注册短信: %w", errSMS)
	}
	log("收到验证码 " + code + "，提交注册")
	info, errReg := reg.registerQuick(sessionID, phone, code)
	if errReg != nil {
		return nil, fmt.Errorf("注册提交失败: %w", errReg)
	}
	return info, nil
}

// claimExisting mirrors SelectAccountContent's 登录 button: SMS (same
// NEW_PORTAL_REGISTER scene the SPA still sends on USED numbers) →
// get-account-info → customer/ticket/quick_login → casLogin redemption later.
func (j *regJob) claimExisting(pool *smsPool, reg *huaweiRegister, phone, sessionID, oldMsg, oldCode string, cfg Config, log func(string)) (*accountInfo, error) {
	if errSend := reg.sendVerifyCode(sessionID, phone); errSend != nil {
		return nil, fmt.Errorf("已注册号发送验证码失败: %w", errSend)
	}
	log("验证码已发送（登录共用接口），等待短信...")
	code, _, errSMS := pool.waitSMS(phone, cfg.SMSWaitMS, oldMsg, oldCode, log)
	if errSMS != nil {
		return nil, fmt.Errorf("等待登录短信: %w", errSMS)
	}
	log("收到验证码 " + code + "，查询绑定账号")

	accounts, errList := reg.getAccountInfoList(sessionID, phone, code)
	if errList != nil {
		return nil, fmt.Errorf("查询已注册账号失败: %w", errList)
	}
	picked, ok := pickAccount(accounts)
	if !ok {
		return nil, fmt.Errorf("该手机号已注册但未查到可登录账号")
	}
	log(fmt.Sprintf("选中账号 %s（%s），申请 quick_login ticket", picked.CustomerName, picked.CustomerID))

	ticket, errTicket := reg.quickLoginTicket(sessionID, picked.CustomerID, code)
	if errTicket != nil {
		return nil, fmt.Errorf("quick_login 取 ticket 失败: %w", errTicket)
	}
	name := picked.CustomerName
	if name == "" {
		name = picked.CustomerID
	}
	return &accountInfo{
		DomainName:  name,
		Ticket:      ticket,
		LoginViaCAS: true,
	}, nil
}
