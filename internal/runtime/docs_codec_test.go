package runtime

import (
	"testing"
)

func TestDocsCodecByteBudget(t *testing.T) {
	code := `export default {fetch(){
 const c=globalThis.__flats_docsCodec;
 const fails=f=>{try{f();return false}catch{return true}};
 const data=new Uint8Array([0,128,255,1]);
 if(c.encode(data.subarray(1,3))!=="gP8=" || c.decode("gP8=").join(",")!=="128,255")throw Error("subarray/roundtrip");
 if(c.length("한🙂")!==7 || c.digest("abc")!=="ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")throw Error("UTF8/hash");
 if(c.digests(["abc","한🙂"])!==c.digest("abc")+c.digest("한🙂") || c.digests([])!=="")throw Error("batched hash");
 for(const s of ["AB==","AA","!!!!","AA==\n","AA==\u0000AAAA"]){if(!fails(()=>c.decode(s)))throw Error("canonical base64")}
 const unicode="한🙂\u0000\ufeff";
 const encoded=c.textEncoder.encode(unicode);
 if(c.textDecoder.decode(encoded)!==unicode)throw Error("UTF8 roundtrip");
 const into=new Uint8Array(20), result=c.textEncoder.encodeInto(unicode,into);
 if(result.read!==unicode.length || result.written!==encoded.length || c.textDecoder.decode(into.subarray(0,result.written))!==unicode)throw Error("UTF8 into");
 if(!fails(()=>c.textDecoder.decode(new Uint8Array([255]))))throw Error("invalid UTF8 accepted");
 const max=new Uint8Array(1536*1024);max[max.length-1]=255;
 if(c.decode(c.encode(max))[max.length-1]!==255)throw Error("maximum roundtrip");
 if(!fails(()=>c.encode(new Uint8Array(1536*1024+1))) || !fails(()=>c.decode("A".repeat(2*1024*1024+4))))throw Error("byte budget");
 if(Object.keys(globalThis).includes("__flats_docsCodec") || typeof __flats_docs_codec!=="undefined")throw Error("internal property");
 return new Response("ok");
 }};`
	f := mustStart(t, newManager(t), "docs-codec", map[string]string{"index.js": code}, "index.js", nil)
	r := f.do(t, "GET", "/", "")
	if r.status != 200 || r.body != "ok" {
		t.Fatal(r.status, r.body, f.logs)
	}
}
