// A key pool's allowance belongs to that key, not the first card returned.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const { test } = require('node:test');
const { chromium, webkit } = require('playwright');
const assets = path.resolve(__dirname, '../assets');
const names = ['1114', '1112', '1108', 'kaixiang'];
const provider = () => ({
  id:'astergate',name:'astergate',icon:'generic',anthropic:'https://relay.test/astergate',chat:'',responses:'',catalog:'',
  models:[{id:'claude-fable-5-2',on:true}],agents:[],fallback:[],headers:{},key:{set:true,masked:'agk_…one'},
  balanceToken:{takes:false,set:false},proxy:'',keyList:names.map((name,i)=>({id:'key'+i,name,masked:'agk_…'+i,on:true,active:!i,usageCap:true})),
});
const card = (user, used) => ({provider:'astergate',user,plan:'max',windows:[
  {name:'5 hours',used,resetsAt:new Date(Date.now()+3600000).toISOString()},
  {name:'7 days',used:used+1}, {name:'Fable · 7 days',used:used+2},
]});
async function setup(browser, lang, width, p=provider(), initial=names.map((n,i)=>card(n,10+i*20))) {
  const context=await browser.newContext({viewport:{width,height:850},reducedMotion:'reduce'});
  const page=await context.newPage(); page.setDefaultTimeout(5000);
  const ctl={cards:initial,reads:[],posts:[],errors:[],fail:false};
  page.on('pageerror',e=>ctl.errors.push(e.message));
  await page.route('**/*',async route=>{
    const u=new URL(route.request().url()),json=data=>route.fulfill({json:data});
    if(u.pathname==='/boot.js')return route.fulfill({contentType:'text/javascript',body:`window.bootPrefs={lang:${JSON.stringify(lang)},theme:'light',web:true};`});
    if(u.pathname==='/wails/runtime.js')return route.fulfill({contentType:'text/javascript',body:'export const Window={};'});
    if(u.pathname==='/api/state')return json({agents:[],profiles:[],settings:{lang,theme:'light',quotaReads:'asked'}});
    if(u.pathname==='/api/providers')return json({providers:[p],presets:[],excluded:[],gateway:{running:true,window:true}});
    if(u.pathname==='/api/groups')return json({groups:[]});
    if(u.pathname==='/api/usage/quotas'){
      ctl.reads.push(u.search);return json(ctl.cards.toReversed());
    }
    if(u.pathname==='/api/usage/quotas/refresh'){
      ctl.posts.push([u.searchParams.get('provider'),u.searchParams.get('user')||'']);
      const user=u.searchParams.get('user')||'';
      ctl.cards=ctl.cards.filter(q=>q.provider!=='astergate'||(q.user||'')!==user);
      ctl.cards.push(ctl.fail?{provider:'astergate',user,error:'upstream unavailable'}:card(user,24));
      return json(ctl.cards);
    }
    if(u.pathname==='/api/settings/quota-left')return json({lang,theme:'light',quotaLeft:route.request().postDataJSON().on,quotaReads:'asked'});
    if(u.pathname.startsWith('/api/'))return json({});
    const f=path.join(assets,u.pathname==='/'?'index.html':u.pathname);
    return route.fulfill({body:await fs.readFile(f).catch(()=>''),contentType:{'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml'}[path.extname(f)]});
  });
  await page.goto('http://gateway.test/?view=providers');
  await page.locator('.row.provider',{hasText:'astergate'}).first().click();
  return {page,context,ctl,row:i=>page.locator(`.editor .acc[data-account-id="key${i}"]`)};
}
for(const [engine,type] of [['chromium',chromium],['webkit',webkit]]) {
 test(`${engine}: key allowances, refresh, remaining, and edits in every language`,async t=>{
  const browser=await type.launch();t.after(()=>browser.close());
  for(const lang of ['en','zh','zh-TW','ja','de'])for(const width of [390,1000]){
    const {page,context,ctl,row}=await setup(browser,lang,width,provider(),[
      ...names.map((n,i)=>card(n,10+i*20)),{...card('1112',99),provider:'other'},card('',98),
    ]);
    try {
      for(let i=0;i<4;i++) {
        await row(i).locator('.aq-w b').first().waitFor();
        assert.match(await row(i).locator('.aq-w b').first().textContent(),new RegExp(`${10+i*20}%`));
        assert.match(await row(i).locator('.aq').getAttribute('title'),/Fable/);
        assert.equal(await row(i).locator('.key-quota-refresh').evaluate(x=>getComputedStyle(x).opacity),'1');
      }
      assert.ok(ctl.reads.length>0);assert.ok(ctl.reads.every(s=>!s.includes('asked')));
      assert.deepEqual(ctl.posts,[], 'opening must not force an upstream read');
      await row(1).locator('.key-quota-refresh').click();
      await page.waitForFunction(()=>document.querySelector('.acc[data-account-id="key1"] .aq-w b')?.textContent.includes('24%'));
      assert.deepEqual(ctl.posts,[['astergate','1112']]);
      assert.match(await row(0).locator('.aq-w b').first().textContent(),/10%/);
      await row(1).locator('.key-quota-mode').click();
      await page.waitForFunction(()=>document.querySelector('.acc[data-account-id="key1"] .aq-w b')?.textContent.includes('76%'));
      assert.match(await row(3).locator('.aq-w b').first().textContent(),/30%/);
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
      // A late quota update must preserve an unsaved key value and its focus.
      await row(1).locator('.key-edit').click();
      const input=row(1).locator('input.key-value-in');await input.fill('unsaved-test-value');
      const top=await page.locator('.editor .ebody').evaluate(x=>x.scrollTop);
      await page.evaluate(()=>{quotas=quotas.map(q=>q.provider==='astergate'&&q.user==='1112'?{...q,windows:[{name:'5 hours',used:25}]}:q);renderQuotas();});
      assert.equal(await input.inputValue(),'unsaved-test-value');assert.equal(await input.evaluate(x=>document.activeElement===x),true);
      assert.equal(await page.locator('.editor .ebody').evaluate(x=>x.scrollTop),top);
      assert.deepEqual(ctl.errors,[]);
    } finally {await context.close();}
  }
 });
 test(`${engine}: missing readings, errors, single key, and ambiguous labels`,async t=>{
  const browser=await type.launch();t.after(()=>browser.close());
  const p=provider();p.keyList[3].on=false;
  const a=await setup(browser,'en',1000,p,[{provider:'astergate',user:'1114',error:'not read yet: magpie reads allowances only when you ask'},card('1112',30)]);
  try {
    await a.row(0).locator('.aq-none').waitFor();
    assert.match(await a.row(0).locator('.aq').textContent(),/Refresh/i);
    assert.match(await a.row(2).locator('.aq').textContent(),/No allowance reported/);
    assert.equal(await a.row(3).locator('.key-quota-refresh').count(),0);
    a.ctl.fail=true;await a.row(1).locator('.key-quota-refresh').click();
    await a.row(1).locator('.aq-none').waitFor();
    assert.equal(await a.row(1).locator('.aq-w').count(),0);
    assert.equal(await a.row(1).locator('.aq').getAttribute('title'),'upstream unavailable');
    assert.deepEqual(a.ctl.errors,[]);
  } finally {await a.context.close();}
  const single=provider();single.keyList=single.keyList.slice(0,1);
  const b=await setup(browser,'en',1000,single,[card('',33),card('1114',99)]);
  try {
    await b.row(0).locator('.aq-w b').first().waitFor();assert.match(await b.row(0).locator('.aq-w b').first().textContent(),/33%/);
    await b.row(0).locator('.key-quota-refresh').click();
    await b.page.waitForFunction(()=>document.querySelector('.key-quota-refresh.busy')===null);
    assert.deepEqual(b.ctl.posts,[['astergate','']]);
  } finally {await b.context.close();}
  const duplicate=provider();duplicate.keyList[1].name='1114';
  const c=await setup(browser,'en',1000,duplicate);
  try {
    await c.row(2).locator('.aq-w b').first().waitFor();
    assert.equal(await c.row(0).locator('.aq').count(),0);assert.equal(await c.row(1).locator('.key-quota-refresh').count(),0);
    assert.deepEqual(c.ctl.errors,[]);
  } finally {await c.context.close();}
 });
}
