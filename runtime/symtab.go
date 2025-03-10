package main

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var memreg *regexp.Regexp = regexp.MustCompile(`^([\&\$\#]*\w+)\s*\[\s*([\&\$\#]*\w+(?:[\.\:]\d+)*)\s*\]`)
var inreg *regexp.Regexp = regexp.MustCompile(`^([\&\$\#]*)(\w+)((?:[\.\:]\d+)*)`)

type varentry struct {
	value         int
	size 		  int //1 mans normal variable, 0 means part of an array, 2+ means head of array and contains size of array
	lastupdatepid int //-1 means its uninit
	lastupdatepc  int //-1 means its uninit
}
type addr struct{
	idx int
	isGlobal bool //if true, refers to M[idx], otherwise refers varmem
	ctype ctype
}
type ctype int
const (
	NONETYPE ctype = iota
	INT
	INTA
	SYNC
)
func ctype_of(t string) ctype{
	switch t{
	case "int":
		return INT
	case "int[]":
		return INTA
	case "sync":
		return SYNC
	default:
		panic("Unknown type")
	}
}
func (c ctype) toString() string{
	switch c{
	case INT:
		return "int"
	case INTA:
		return "int[]"
	case SYNC:
		return "sync"
	default:
		panic("Unknown type")
	}
}
type symtab struct {
	//Value of type varentry
	alloctable map[string]addr
	varmem map[int]varentry
	heapmem   map[int]int
	heapuppid map[int]int
	heapuppc map[int]int 
	semconds map[string]*sync.Cond
	alclimit int
}


func newSymtab() *symtab {
	tab := symtab{}
	tab.alloctable = make(map[string]addr)
	tab.varmem = make(map[int]varentry)
	tab.heapmem = make(map[int]int)
	tab.heapuppid = make(map[int]int)
	tab.heapuppc = make(map[int]int)
	tab.semconds = make(map[string]*sync.Cond)
	tab.alclimit = 1
	return &tab
}
func (r *runtime) NotifySem(name string){
	v,ex := r.semconds[name]
	if !ex{
		v = sync.NewCond(exmut)
		r.semconds[name] = v
	}
	v.Signal()
}
func (r *runtime) WaitSem(name string) bool{
	//this infinity loops but it should be okay cuz if it does program executes more V than P thereby its very wrong
	//return true if its done waiting, false if terminated
	v,ex := r.semconds[name]
	if !ex{
		v = sync.NewCond(exmut)
		r.semconds[name] = v
	}
	semtolisten := make(chan bool)
	go func(){
		v.Wait()
		semtolisten <- true
	}()
	select {
	case <- r.runclose:
		r.runclose <- true
		return false
	case <- r.rundoner:
		r.rundoner <- true
		return false
	case <- semtolisten:
		return true
	}
	
}
func (r *runtime) AllocSym(s string, t ctype, size int) int{
	//allocate new entry in alloctable, does not update pid and pc
	l := r.alclimit
	r.alloctable[s] = addr{l,false,t}
	for i := range size{
		r.symtab.varmem[l+i] = varentry{0,size,-1,-1}
	}
	r.alclimit += size
	return l
}

func (r *runtime) Castable(from ctype, to ctype) bool {
	if from == NONETYPE{
		return true
	}
	return from == to
}

func (r *runtime) UnaryOf(t ctype) ctype{
	if t == INTA{
		return INT
	}
	return t
}

func (r *runtime) GetAddr(name string, pc *pc) (addr,int,string){
	match := memreg.FindStringSubmatch(name)
	if match != nil{
		idx,er := strconv.Atoi(match[2])
		if er != nil{
			v,t := r.ReadSym(match[2],pc)
			if t != INT{
				panic("Read of indexed value of non-int variable")
			}
			idx = v
		}
		if match[1] == "M"{
			return addr{idx,true,NONETYPE},0,"M"
		}
		aname := r.GetAllocName(name,pc)
		return r.alloctable[aname],idx,match[1]
	}
	aname := r.GetAllocName(name,pc)
	return r.alloctable[aname],0,name
}

func (r *runtime) ReadAddr(a addr, offset int, dagname string, pc *pc) int{
	if a.ctype != INTA && offset != 0{
		panic("Read of indexed value of non-int variable")
	}
	dn := dagname
	if a.isGlobal{
		v := r.symtab.heapmem[a.idx+offset]
		if a.ctype == INTA{
			dn += "["
			dn += strconv.Itoa(offset)
			dn += "]"
		}
		if !rev {
			r.addEdge(r.heapuppid[a.idx+offset],r.heapuppc[a.idx+offset],dn,false,pc.pid,pc.pc)
		}
		fmt.Printf("ReadOp: %s -> M[%d] -> %d\n",dn,a.idx+offset,v)
		return v
	}
	v := r.symtab.varmem[a.idx+offset]
	if a.ctype == INTA{
		dn += "["
		dn += strconv.Itoa(offset)
		dn += "]"
	}
	if !rev{
		r.addEdge(v.lastupdatepid,v.lastupdatepc,dn,false,pc.pid,pc.pc)
	}
	fmt.Printf("ReadOp: %s -> Var[%d] -> %d\n",dn,a.idx+offset,v.value)
	return v.value
}
func (r *runtime) WriteAddr(a addr, val int, offset int, dagname string, pc *pc){
	if a.ctype != INTA && offset != 0{
		panic("Write of indexed value of non-int variable")
	}
	dn := dagname
	if a.isGlobal{
		v := r.symtab.heapmem[a.idx+offset]
		if a.ctype == INTA{
			dn += "["
			dn += strconv.Itoa(offset)
			dn += "]"
		}
		if !rev{
			r.addEdge(r.heapuppid[a.idx+offset],r.heapuppc[a.idx+offset],dn,true,pc.pid,pc.pc)
			r.heapuppid[a.idx+offset] = pc.pid
			r.heapuppc[a.idx+offset] = pc.pc
		} else {
			lpid, lpc := r.GetLastDag(dn,pc)
			r.heapuppid[a.idx+offset] = lpid
			r.heapuppc[a.idx+offset] = lpc
		}
		fmt.Printf("WriteOp: %s -> M[%d] -> %d\n",dn,a.idx+offset,v)
		r.symtab.heapmem[a.idx+offset] = val
		return
	}
	v := r.symtab.varmem[a.idx+offset]
	if a.ctype == INTA{
		dn += "["
		dn += strconv.Itoa(offset)
		dn += "]"
	}
	if !rev{
		r.addEdge(v.lastupdatepid,v.lastupdatepc,dn,true,pc.pid,pc.pc)
		r.symtab.varmem[a.idx+offset] = varentry{val,v.size,pc.pid,pc.pc}
	} else {
		lpid, lpc := r.GetLastDag(dn,pc)
		r.symtab.varmem[a.idx+offset] = varentry{val,v.size,lpid,lpc}
	}
	fmt.Printf("WriteOp: %s -> Var[%d] -> %d\n",dn,a.idx+offset,v.value)
}
func (r *runtime) GetLastDag(name string, pc *pc)(lpid int,lpc int){
	lpid = -1
	lpc = -1
	if pc.pid >= len(r.dag.incedges) {
		return
	}
	if pc.pc >= len(r.dag.incedges[pc.pid]) {
		return
	}
	for _, edge := range r.dag.incedges[pc.pid][pc.pc] {
		if edge.wt && edge.varname == name {
			lpid = edge.pid
			lpc = edge.pc
		}
	}
	return
}
func (r *runtime) SetSym(dst string, src string, t ctype, pc *pc){
	srca,_,_ := r.GetAddr(src,pc)
	if !r.Castable(srca.ctype,t){
		panic("Mismatched type")
	}
	dstname,_,_ := r.convert(dst,pc)
	dsta,ex := r.symtab.alloctable[dstname]
	if ex && dsta.idx != 0{
		panic("Set assertion fail")
	}
	r.symtab.alloctable[dstname] = addr{srca.idx,srca.isGlobal,t}	
}
func (r *runtime) UnsetSym(dst string, src string, t ctype, pc *pc){
	srca,_,_ := r.GetAddr(src,pc)
	if !r.Castable(srca.ctype,t){
		panic("Mismatched type")
	}
	dstname,_,_ := r.convert(dst,pc)
	dsta,ex := r.symtab.alloctable[dstname]
	if !ex || dsta.idx != srca.idx || dsta.isGlobal != srca.isGlobal{
		panic("Unset assertion fail")
	}
	r.symtab.alloctable[dstname] = addr{}
}
func (r *runtime) ReadSym(s string, pc *pc) (int, ctype) {
	adr,idx,name := r.GetAddr(s,pc)
	v := r.ReadAddr(adr,idx,name,pc)
	return v,r.UnaryOf(adr.ctype)
}
func (r *runtime) GetAllocName(s string,pc *pc) string {
	//name of variable, index
	key,ct,alc := r.convert(s,pc)
	for {
		_, ex := r.symtab.alloctable[key]
		if ex {
			return key
		}
		if !ct {
			if alc {
				r.AllocSym(key,INT,1) 
				return key
			}
			panic("unknown variable")
		}
		key,ex = r.cut(key)
		if !ex{
			if alc {
				r.AllocSym(key,INT,1) 
				return key
			}
			panic("unknown variable")
		}
	}
}
func (r *runtime) GetAllocNameOrAlloc(s string,pc *pc, t ctype,size int) string {
	//name of variable, index
	key,ct,alc := r.convert(s,pc)
	for {
		_, ex := r.symtab.alloctable[key]
		if ex {
			return key
		}
		if !ct && alc{
			r.AllocSym(key,t,size) 
			return key
		}
		key,ex = r.cut(key)
		if !ex && alc{
			r.AllocSym(key,t,size)
			return key
		}
	}
}

func (r *runtime) WriteSym(s string, value int, pc *pc) {
	adr,idx,name := r.GetAddr(s,pc)
	r.WriteAddr(adr,value,idx,name,pc)
}

//$ means its newly allocated one, and should not search downwards
//& means its ptr to the variable
//# will be allocated directly
func (tab *symtab) convert(name string, pc *pc) (string,bool,bool) {
	//b1 = false if name should not be cut
	//b2 = true if allocatable
	match := inreg.FindStringSubmatch(name)
	if match != nil{
		s := match[2]
		if !strings.Contains(match[1],"#"){
			for _,v := range pc.stackdepth{
				s += ":"
				for i,v1 := range v{
					if i != 0{
						s += "."
					}
					s += strconv.Itoa(v1)
				}
			}
		}
		s += match[3]
		return s,!strings.Contains(match[1],"$"),strings.Contains(match[1],"#")||strings.Contains(match[1],"$")
	}
	panic("unknown variable")
}
func (tab *symtab) cut(name string) (string,bool) {
	//convert n.0.0 -> n.0, return false if cannot cut
	i := strings.LastIndexAny(name,".:")
	if i != -1{
		return name[:i],true
	}
	return name,false
	
}

func (tab *symtab) PrintSym() {
	fmt.Print("---Symbol Status---\n")
	tkeys := make([]string,0,len(tab.alloctable))

	for i := range tab.alloctable {	
		tkeys = append(tkeys, i)
	}
	slices.SortFunc(tkeys,func (x string, y string) int {
		return cmp.Compare(len(x),len(y))
	})
	for _,i := range tkeys{
		e := tab.alloctable[i]
		if e.idx != 0 || e.isGlobal{
			if !e.isGlobal{
				v := tab.varmem[e.idx]
				if v.size == 1 && !strings.Contains(i,"tmp"){
					fmt.Printf("%s:%s -> Var[%d] -> %d\n",i,e.ctype.toString(),e.idx,v.value)
				}
			} else {
				v := tab.heapmem[e.idx]
				fmt.Printf("%s:%s -> M[%d] -> %d\n",i,e.ctype.toString(),e.idx,v)
			}
		}
	}
	mkeys := make([]int,0,len(tab.heapmem))
	for i := range tab.heapmem{
		mkeys = append(mkeys, i)
	}
	mx := slices.Max(mkeys)
	fmt.Print("\n---Memory Status---\n")
	fmt.Print("[")
	for i := range mx+1{
		if i != 0 {
			fmt.Print(",")
		}
		fmt.Print(tab.heapmem[i])
	}
	fmt.Print("]\n")
}
