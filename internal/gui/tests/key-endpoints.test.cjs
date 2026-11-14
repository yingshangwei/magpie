const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const { test } = require('node:test');
const { chromium, webkit } = require('playwright');
const assets = path.resolve(__dirname, '../assets');
for (const engine of process.env.BROWSER ? [process.env.BROWSER] : ['chromium', 'webkit']) {
 for (const lang of ['en','zh','zh-TW','ja','de']) {
  test(`${engine} ${lang}: account URL survives reload and can follow provider again`, async t => {
   const browser = await (engine === 'webkit' ? webkit.launch() : chromium.launch());
   t.after(()=>browser.close());
   const page = await browser.newPage({viewport:{width:440,height:850},reducedMotion:'reduce'});
   page.setDefaultTimeout(10000);
   const errors=[];page.on('pageerror',e=>errors.push(e.message));
   const p={id:'astergate',name:'astergate',icon:'generic',baseAPI:'anthropic',anthropic:'https://site-a.test/astergate',chat:'',responses:'',modelsURL:'https://site-a.test/astergate/v1/models',catalog:'',models:[{id:'claude-opus-5-5',on:true}],agents:[],fallback:[],headers:{},key:{set:true,masked:'agk_…one'},balanceToken:{takes:false,set:false},keyList:[{id:'aaaaaaaaaa',name:'1114',masked:'agk_…one',on:true,active:true},{id:'bbbbbbbbbb',name:'Kaixiang',masked:'agk_…two',on:true}],accountEndpoints:{bbbbbbbbbb:{anthropic:'https://beta.astergate.cc'}},proxy:''};
   const posts=[];
   const list=()=>({providers:[p],presets:[],excluded:[],gateway:{running:true,window:true}});
   await page.route('**/*',async route=>{
    const u=new URL(route.request().url()),json=data=>route.fulfill({json:data});
    if(u.pathname==='/boot.js')return route.fulfill({contentType:'text/javascript',body:`window.bootPrefs={lang:${JSON.stringify(lang)},theme:'light',web:true};`});
    if(u.pathname==='/wails/runtime.js')return route.fulfill({contentType:'text/javascript',body:'export const Window={};'});
    if(u.pathname==='/api/state')return json({agents:[],profiles:[],settings:{lang,theme:'light'}});
    if(u.pathname==='/api/providers')return json(list());
    if(u.pathname==='/api/groups')return json({groups:[]});
    if(u.pathname==='/api/keys/endpoints'){
     const body=route.request().postDataJSON();posts.push(body);
     if(Object.keys(body.endpoints).length)p.accountEndpoints[body.ref]=body.endpoints;else delete p.accountEndpoints[body.ref];
     return json(list());
    }
    if(u.pathname.startsWith('/api/'))return json({});
    const f=path.join(assets,u.pathname==='/'?'index.html':u.pathname);
    return route.fulfill({body:await fs.readFile(f).catch(()=>''),contentType:{'.html':'text/html','.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml'}[path.extname(f)]});
   });
   await page.goto('http://gateway.test/?view=providers');
   await page.locator('.row.provider',{hasText:'astergate'}).first().click();
   const row=page.locator('.editor .acc[data-account-id="bbbbbbbbbb"]');
   const button=row.locator('.key-endpoint');
   await page.waitForFunction(()=>document.querySelector('.editor .acc[data-account-id="bbbbbbbbbb"] .key-endpoint')?.disabled===false);
   assert.match(await button.getAttribute('title'),/beta\.astergate\.cc/);
   await button.click();const input=row.locator('input.rename-in');
   assert.equal(await input.inputValue(),'https://beta.astergate.cc');
   await input.fill('https://new.test/team/astergate');await input.press('Enter');
   await page.waitForFunction(()=>document.querySelector('.editor .acc[data-account-id="bbbbbbbbbb"] .key-endpoint')?.title.includes('new.test'));
   assert.equal(posts[0].ref,'bbbbbbbbbb');assert.equal(posts[0].endpoints.anthropic,'https://new.test/team/astergate');assert.equal(posts[0].endpoints.modelsURL,'');
   assert.equal(p.anthropic,'https://site-a.test/astergate');
   await page.reload();
   await page.waitForFunction(()=>document.querySelector('.editor .acc[data-account-id="bbbbbbbbbb"] .key-endpoint')?.disabled===false);
   assert.match(await button.getAttribute('title'),/new\.test/);
   await button.click();await input.fill('');await input.press('Enter');
   await page.waitForFunction(()=>document.querySelector('.editor .acc[data-account-id="bbbbbbbbbb"] .key-endpoint')?.title.includes('site-a.test'));
   assert.deepEqual(posts.at(-1).endpoints,{});assert.deepEqual(errors,[]);
   assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  });
 }
}
