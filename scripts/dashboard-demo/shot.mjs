import fs from 'fs';
const [,, outDir, theme='light', width='1440', prefix=''] = process.argv;
const W = +width, BASE='http://127.0.0.1:'+(process.env.PORT||4152)+'/dashboard/';
const pages = JSON.parse(fs.readFileSync('pages.json','utf8'));
const j = await (await fetch('http://127.0.0.1:9334/json/new?about:blank',{method:'PUT'})).json();
const ws = new WebSocket(j.webSocketDebuggerUrl); await new Promise(r=>ws.onopen=r);
let id=0; const pend=new Map();
ws.onmessage=e=>{const m=JSON.parse(e.data); if(m.id&&pend.has(m.id)){pend.get(m.id)(m);pend.delete(m.id)}};
const send=(method,params={})=>new Promise(r=>{const i=++id;pend.set(i,r);ws.send(JSON.stringify({id:i,method,params}))});
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
await send('Emulation.setDeviceMetricsOverride',{width:W,height:900,deviceScaleFactor:1,mobile:W<600});
await send('Emulation.setEmulatedMedia',{features:[{name:'prefers-color-scheme',value:theme}]});
await send('Page.enable');
for (const [name,hash,wait] of pages){
  await send('Page.navigate',{url:BASE+hash});
  await send('Runtime.evaluate',{expression:'location.reload()'});
  await sleep(wait||7000);
  const m=(await send('Page.getLayoutMetrics')).result;
  const h=Math.min(Math.ceil(m.cssContentSize.height),4500);
  await send('Emulation.setDeviceMetricsOverride',{width:W,height:h,deviceScaleFactor:1,mobile:W<600});
  await sleep(400);
  const r=await send('Page.captureScreenshot',{format:'png'});
  fs.writeFileSync(`${outDir}/${prefix}${name}.png`,Buffer.from(r.result.data,'base64'));
  await send('Emulation.setDeviceMetricsOverride',{width:W,height:900,deviceScaleFactor:1,mobile:W<600});
  console.log(name,h);
}
ws.close(); process.exit(0);
