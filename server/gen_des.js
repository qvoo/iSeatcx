// 从学校页面 des.js 自动导出 Go 表（避免手抄出错），输出 des_gen.go
const fs = require('fs')
const vm = require('vm')
const src = fs.readFileSync('des.js', 'utf8')
const ctx = { console }
vm.createContext(ctx)
vm.runInContext(src, ctx)

const idOf = (n) => Array.from({ length: n }, (_, i) => i)
const initP = ctx.initPermute(idOf(64))          // 64 个来源下标
const expandP = ctx.expandPermute(idOf(32))      // 48 个
const pP = ctx.pPermute(idOf(32))                // 32 个
const finalP = ctx.finallyPermute(idOf(64))      // 64 个

// s1..s8
const boxes = []
for (let i = 1; i <= 8; i++) {
  const re = new RegExp('var s' + i + '\\s*=\\s*(\\[[\\s\\S]*?\\]\\]);')
  const m = src.match(re)
  if (!m) throw new Error('找不到 s' + i)
  boxes.push(eval('(' + m[1] + ')'))
}

// PC-2: 从 generateKeys 里抽取 tempKey[n] = key[m]
const pc2 = new Array(48)
const re2 = /tempKey\[\s*(\d+)\]\s*=\s*key\[\s*(\d+)\]/g
let mm
while ((mm = re2.exec(src)) !== null) pc2[Number(mm[1])] = Number(mm[2])

const arr = (a) => a.join(', ')
let out = 'package main\n\n// 由 tools/des.js 自动生成（该校统一认证页面用的非标准 DES 表），请勿手改。\n\n'
out += 'var genInitPermute = []int{' + arr(initP) + '}\n\n'
out += 'var genExpandPermute = []int{' + arr(expandP) + '}\n\n'
out += 'var genPPermute = []int{' + arr(pP) + '}\n\n'
out += 'var genFinalPermute = []int{' + arr(finalP) + '}\n\n'
out += 'var genPC2 = []int{' + arr(pc2) + '}\n\n'
out += 'var genSBox = [8][4][16]int{\n'
for (const b of boxes) {
  out += '\t{\n'
  for (const row of b) out += '\t\t{' + arr(row) + '},\n'
  out += '\t},\n'
}
out += '}\n'
fs.writeFileSync('des_gen.go', out)
console.log('已生成 des_gen.go：IP', initP.length, 'E', expandP.length, 'P', pP.length, 'FP', finalP.length, 'PC2', pc2.filter((x) => x !== undefined).length)
