package main

import(
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"bytes"
	"encoding/json" 
	"net"
	"net/http"
	"net/http/httputil"
	"bufio"  
	"encoding/binary"
	"sync"
	"time"
	"strconv"
	"flag"
	"errors"
	"runtime"
	"unsafe"
	"syscall"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
) 

var (
	REMOTE_IP = ""
	API_VERSION = ""
	TOKEN = "" 
	CMD = "" 
	PROTOCAL = ""
)


type ExecCreated struct {
    Id string
}

type ExecConfig struct{
	AttachStdin bool
	AttachStdout bool
	AttachStderr bool
	Tty bool
	Cmd []string
	Container string
	Detach bool
	DetachKeys string
} 

type HijackedResponse struct {
	Conn   net.Conn
	Reader *bufio.Reader
}

type DockerCli struct { 
	// in holds the input stream and closer (io.ReadCloser) for the client.
	in io.ReadCloser
	// out holds the output stream (io.Writer) for the client.
	out io.Writer
	// err holds the error stream (io.Writer) for the client.
	err io.Writer 
	// inFd holds the file descriptor of the client's STDIN (if valid).
	inFd uintptr
	// outFd holds file descriptor of the client's STDOUT (if valid).
	outFd uintptr
	// isTerminalIn indicates whether the client's STDIN is a TTY
	isTerminalIn bool
	// isTerminalOut indicates whether the client's STDOUT is a TTY
	isTerminalOut bool 
	// state holds the terminal input state
	inState *State
	// outState holds the terminal output state
	outState *State
}

// StatusError reports an unsuccessful exit by a command.
type StatusError struct {
		Status     string
		StatusCode int
}

func main(){
	remote := flag.String("remote", "", "remote docker api ip:port")
	version := flag.String("version", "", "remote docker api version")
	container := flag.String("container", "", "container id")
        cmd := flag.String("cmd", "/bin/sh", "cmd")
	protocal := flag.String("protocal", "tcp", "protocal")
	flag.Parse()	
	
	REMOTE_IP = *remote
	API_VERSION = *version
	if REMOTE_IP==""{
		fmt.Println("empty remote ip, usage: myexec -remote=xx -version=xx -container=xx [-cmd=xx[,xx] -protocal=xx]")
		return
	}
        if API_VERSION==""{
                fmt.Println("empty remote api version, usage: myexec -remote=xx -version=xx -container=xx [-cmd=xx[,xx] -protocal=xx]")
                return
        }
        if *container==""{
                fmt.Println("empty container id, usage: myexec -remote=xx -version=xx -container=xx [-cmd=xx[,xx] -protocal=xx]")
                return
        }
	if *cmd!=""{
		CMD = *cmd
	}
	if *protocal!=""{
                PROTOCAL = *protocal
        }
	CmdExec(*container)
}

var (
	execConfig = flag.String("config", "./conf.json", "Specifies the exec configuration file")
)

type ConfigJson struct{
	Token string
}

func init(){
	data, err := ioutil.ReadFile(*execConfig)
	if err != nil{
		panic(err)
	}

	var v ConfigJson
	datajson := []byte(data)
	err = json.Unmarshal(datajson, &v)
	if err != nil{
		panic(err)
	}
	fmt.Println("Token = "+v.Token)
	TOKEN = v.Token
}

func CmdExec(container string) error { 
	execConfig := &ExecConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          []string{CMD},
		Detach:       false,
 	   	DetachKeys:	  "ctrl-p,ctrl-q"}

	// Interactive exec requested.
	var (
		out, stderr io.Writer
		in          io.ReadCloser
		errCh       chan error
	)
 
	in = os.Stdin 
	out = os.Stdout 
	stderr = os.Stderr  

	var cli DockerCli 
	cli.in = in
	cli.out = out
	cli.err = stderr
	cli.inFd, cli.isTerminalIn = term_getFdInfo(in)
	cli.outFd, cli.isTerminalOut = term_getFdInfo(out)
	
	ctx := context.Background()

	headers := map[string]string{"Token": TOKEN}
	response, err := cli.execCreate(ctx,container,execConfig,headers)
	if err != nil {
		fmt.Println(err)
		return err
	}

	execID := response.Id
	if execID == "" {
		fmt.Println("exec ID empty")
		return nil
	}
 
	resp, err := cli.containerExecAttach(ctx,execID,execConfig,headers)
	if err != nil {
		return err
	}
	defer resp.Conn.Close()

	errCh = promise_Go(func() error {
		return cli.holdHijackedConnection(ctx, execConfig.Tty, in, out, stderr, resp)
	})

	if execConfig.Tty && cli.isTerminalIn {
		if err := cli.monitorTtySize(ctx, execID, true, headers); err != nil {
			fmt.Fprintf(cli.err, "Error monitoring TTY size: %s\n", err)
		}
	}

	if err := <-errCh; err != nil {
		fmt.Println("Error hijack: %s", err)
		return err
	}

	var status int
	if _, status, err = cli.getExecExitCode(ctx, execID, headers); err != nil {
		return err
	}

	if status != 0 {
		return errors.New("error, status: "+strconv.Itoa(status))
	}

	return nil
} 

func (cli *DockerCli) execCreate(ctx context.Context, container string, body interface{}, headers map[string]string) (resp ExecCreated, err error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return ExecCreated{},fmt.Errorf("error marshaling exec config: %s", err)
	} 
	rdr := bytes.NewReader(buf)

	req, err := http.NewRequest("POST", "/"+API_VERSION+"/containers/"+container+"/exec", rdr)
	if err != nil {
		return ExecCreated{},fmt.Errorf("error during exec request: %s", err)
	} 

	req.Header.Set("User-Agent", "Docker-Client")
	req.Header.Set("Content-Type", "application/json") 
	for k,v:=range headers{
		req.Header.Set(k,v)
	} 
	req.Host = REMOTE_IP

	var (
		dial          net.Conn
		dialErr       error 
	)
  
	dial, dialErr = net.Dial(PROTOCAL, REMOTE_IP) 
	if dialErr != nil {
		return ExecCreated{},dialErr
	}

	clientconn := httputil.NewClientConn(dial, nil)
	defer clientconn.Close()

	// Server the connection, error 'connection closed' expected
	response, err := clientconn.Do(req)
	if err != nil {
		return ExecCreated{},err
	}

	out,_ := ioutil.ReadAll(response.Body)
	var s ExecCreated
	err = json.Unmarshal([]byte(out), &s)
	if err != nil {
		return ExecCreated{},err
	}
	fmt.Println("exec ID = ",s.Id)
	return s, nil
	 
	return ExecCreated{},nil
}

// containerExecAttach sends a POST request and hijacks the connection.
func (cli *DockerCli) containerExecAttach(ctx context.Context, execID string, body interface{}, headers map[string]string) (HijackedResponse, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return HijackedResponse{}, err
	}

	rdr := bytes.NewReader(buf)

	req, err := http.NewRequest("POST", "/"+API_VERSION+"/exec/"+execID+"/start", rdr)
	if err != nil {
		return HijackedResponse{},fmt.Errorf("error during exec request: %s", err)
	} 

	req.Header.Set("User-Agent", "Docker-Client")
	req.Header.Set("Content-Type", "application/json") 
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	for k,v:=range headers{
		req.Header.Set(k,v)
	} 
	req.Host = REMOTE_IP
 
	var (
		dialErr       error 
	)
  
	conn, dialErr := net.Dial(PROTOCAL, REMOTE_IP) 
	if dialErr != nil {
		return HijackedResponse{}, dialErr
	}
 
	// When we set up a TCP connection for hijack, there could be long periods
	// of inactivity (a long running command with no output) that in certain
	// network setups may cause ECONNTIMEOUT, leaving the client in an unknown
	// state. Setting TCP KeepAlive on the socket connection will prohibit
	// ECONNTIMEOUT unless the socket connection truly is broken
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	clientconn := httputil.NewClientConn(conn, nil)
	defer clientconn.Close()

	// Server hijacks the connection, error 'connection closed' expected
	_, err = clientconn.Do(req)

	rwc, br := clientconn.Hijack()

	return HijackedResponse{Conn: rwc, Reader: br}, err
}


// ContainerExecInspect holds information returned by exec inspect.
type ContainerExecInspect struct {
	ExecID      string
	ContainerID string
	Running     bool
	ExitCode    int
}
 
func (cli *DockerCli) containerExecInspect(ctx context.Context, execID string, headers map[string]string) (resp ContainerExecInspect, err error) {
	req, err := http.NewRequest("GET", "/"+API_VERSION+"/exec/"+execID+"json", nil)
	if err != nil {
		return ContainerExecInspect{},fmt.Errorf("error during inspect request: %s", err)
	} 

	req.Header.Set("User-Agent", "Docker-Client")
	req.Header.Set("Content-Type", "application/json") 
	for k,v:=range headers{
		req.Header.Set(k,v)
	} 
	req.Host = REMOTE_IP

	var (
		dial          net.Conn
		dialErr       error 
	)
  
	dial, dialErr = net.Dial(PROTOCAL, REMOTE_IP) 
	if dialErr != nil {
		return ContainerExecInspect{},dialErr
	}

	clientconn := httputil.NewClientConn(dial, nil)
	defer clientconn.Close()

	// Server the connection, error 'connection closed' expected
	response, err := clientconn.Do(req)
	if err != nil {
		return ContainerExecInspect{},err
	}

	out,_ := ioutil.ReadAll(response.Body)
	var s ContainerExecInspect
	err = json.Unmarshal([]byte(out), &s)
	if err != nil {
		return ContainerExecInspect{},err
	} 
	return s, nil
}


func (cli *DockerCli) containerExecResize(ctx context.Context, execID string, options ResizeOptions, headers map[string]string) (err error) {
	req, err := http.NewRequest("POST", "/"+API_VERSION+"/exec/"+execID+"/resize?h="+strconv.Itoa(options.Height)+"&w="+strconv.Itoa(options.Width), nil)
	if err != nil {
		return fmt.Errorf("error during resize request: %s", err)
	} 

	req.Header.Set("User-Agent", "Docker-Client")
	req.Header.Set("Content-Type", "text/plain") 
	for k,v:=range headers{
		req.Header.Set(k,v)
	} 
	req.Host = REMOTE_IP

	var (
		dial          net.Conn
		dialErr       error 
	)
  
	dial, dialErr = net.Dial(PROTOCAL, REMOTE_IP) 
	if dialErr != nil {
		return dialErr
	}

	clientconn := httputil.NewClientConn(dial, nil)
	defer clientconn.Close()

	// Server the connection, error 'connection closed' expected
	_, err = clientconn.Do(req)
	if err != nil {
		return err
	}
	 
	return nil
}


func (cli *DockerCli) containerResize(ctx context.Context, containerID string, options ResizeOptions, headers map[string]string) (err error) {
	req, err := http.NewRequest("POST", "/"+API_VERSION+"/containers/"+containerID+"/resize?h="+strconv.Itoa(options.Height)+"&w="+strconv.Itoa(options.Width), nil)
	if err != nil {
		return fmt.Errorf("error during resize request: %s", err)
	} 

	req.Header.Set("User-Agent", "Docker-Client")
	req.Header.Set("Content-Type", "text/plain") 
	for k,v:=range headers{
		req.Header.Set(k,v)
	} 
	req.Host = REMOTE_IP

	var (
		dial          net.Conn
		dialErr       error 
	)
  
	dial, dialErr = net.Dial(PROTOCAL, REMOTE_IP) 
	if dialErr != nil {
		return dialErr
	}

	clientconn := httputil.NewClientConn(dial, nil)
	defer clientconn.Close()

	// Server the connection, error 'connection closed' expected
	_, err = clientconn.Do(req)
	if err != nil {
		return err
	}
	 
	return nil
}

// CloseWriter is an interface that implements structs
// that close input streams to prevent from writing.
type CloseWriter interface {
	CloseWrite() error
}

// HoldHijackedConnection handles copying input to and output from streams to the
// connection
func (cli *DockerCli) holdHijackedConnection(ctx context.Context, tty bool, inputStream io.ReadCloser, outputStream, errorStream io.Writer, resp HijackedResponse) error {
	var (
		err         error
		restoreOnce sync.Once
	)
	if inputStream != nil && tty {
		if err := cli.setRawTerminal(inputStream,outputStream); err != nil {
			return err
		}
		defer func() {
			restoreOnce.Do(func() {
				cli.restoreTerminal(inputStream)
			})
		}()
	}

	receiveStdout := make(chan error, 1)
	if outputStream != nil || errorStream != nil {
		go func() {
			// When TTY is ON, use regular copy
			if tty && outputStream != nil {
				_, err = io.Copy(outputStream, resp.Reader)
				// we should restore the terminal as soon as possible once connection end
				// so any following print messages will be in normal type.
				if inputStream != nil {
					restoreOnce.Do(func() {
						cli.restoreTerminal(inputStream)
					})
				}
			} else {
				_, err = stdCopy(outputStream, errorStream, resp.Reader)
			}

			fmt.Println("[hijack] End of stdout")
			receiveStdout <- err
		}()
	}

	stdinDone := make(chan struct{})
	go func() {
		if inputStream != nil {
			io.Copy(resp.Conn, inputStream)
			// we should restore the terminal as soon as possible once connection end
			// so any following print messages will be in normal type.
			if tty {
				restoreOnce.Do(func() {
					cli.restoreTerminal(inputStream)
				})
			}
			fmt.Println("[hijack] End of stdin")
		}

		if conn, ok := resp.Conn.(CloseWriter); ok { 
			if err := conn.CloseWrite(); err != nil {
				fmt.Println("Couldn't send EOF: %s", err)
			}
		}
		close(stdinDone)
	}()

	select {
	case err := <-receiveStdout:
		if err != nil {
			fmt.Println("Error receiveStdout: %s", err)
			return err
		}
	case <-stdinDone:
		if outputStream != nil || errorStream != nil {
			select {
			case err := <-receiveStdout:
				if err != nil {
					fmt.Println("Error receiveStdout: %s", err)
					return err
				}
			case <-ctx.Done():
			}
		}
	case <-ctx.Done():
	}

	return nil
}

// Go is a basic promise implementation: it wraps calls a function in a goroutine,
// and returns a channel which will later return the function's return value.
func promise_Go(f func() error) chan error {
	ch := make(chan error, 1)
	go func() {
		ch <- f()
	}()
	return ch
}

func (cli *DockerCli) setRawTerminal(in, out interface{}) error {
	if os.Getenv("NORAW") == "" {
		if cli.isTerminalIn {
			state, err := term_setRawTerminalIn(cli.inFd)
			if err != nil {
				return err
			} 
			cli.inState = state
		}
		if cli.isTerminalOut {
			state, err := term_setRawTerminalOutput(cli.outFd)
			if err != nil {
				return err
			} 
			cli.outState = state
		}
	}
	return nil
}


func (cli *DockerCli) restoreTerminal(in io.Closer) error {
	if cli.inState != nil {
		term_restoreTerminal(cli.inFd, cli.inState)
	}
	if cli.outState != nil {
		term_restoreTerminal(cli.outFd, cli.outState)
	}
	// WARNING: DO NOT REMOVE THE OS CHECK !!!
	// For some reason this Close call blocks on darwin..
	// As the client exists right after, simply discard the close
	// until we find a better solution.
	if in != nil && runtime.GOOS != "darwin" {
		return in.Close()
	}
	return nil
}







type ResizeOptions struct{
	Height int
	Width int
}

func (cli *DockerCli) resizeTty(ctx context.Context, execID string, isExec bool, headers map[string]string) {
	height, width := cli.getTtySize()
	cli.resizeTtyTo(ctx, execID, height, width, isExec, headers)
}

// ResizeTtyTo resizes tty to specific height and width
// TODO: this can be unexported again once all container related commands move to package container
func (cli *DockerCli) resizeTtyTo(ctx context.Context, execID string, height, width int, isExec bool, headers map[string]string) {
	if height == 0 && width == 0 {
		return
	}

	options := ResizeOptions{
		Height: height,
		Width:  width,
	}

	var err error
	if isExec {
		err = cli.containerExecResize(ctx, execID, options, headers)
	} else {
		err = cli.containerResize(ctx, execID, options, headers)
	}

	if err != nil {
		fmt.Println("Error resize: %s", err)
	}
}

var ErrConnectionFailed = errors.New("Cannot connect to the Docker daemon. Is the docker daemon running on this host?")
// getExecExitCode perform an inspect on the exec command. It returns
// the running state and the exit code.
func (cli *DockerCli) getExecExitCode(ctx context.Context, execID string, headers map[string]string) (bool, int, error) {
	resp, err := cli.containerExecInspect(ctx, execID, headers)
	if err != nil {
		// If we can't connect, then the daemon probably died.
		if err != ErrConnectionFailed {
			return false, -1, err
		}
		return false, -1, nil
	}

	return resp.Running, resp.ExitCode, nil
}

// MonitorTtySize updates the container tty size when the terminal tty changes size
func (cli *DockerCli) monitorTtySize(ctx context.Context, execID string, isExec bool, headers map[string]string) error {
	cli.resizeTty(ctx, execID, isExec, headers)

	if runtime.GOOS == "windows" {
		go func() {
			prevH, prevW := cli.getTtySize()
			for {
				time.Sleep(time.Millisecond * 250)
				h, w := cli.getTtySize()

				if prevW != w || prevH != h {
					cli.resizeTty(ctx, execID, isExec, headers)
				}
				prevH = h
				prevW = w
			}
		}()
	} else {
		sigchan := make(chan os.Signal, 1)
		//no win
		SIGWINCH := syscall.SIGWINCH
		signal.Notify(sigchan, SIGWINCH)
		go func() {
			for range sigchan {
				cli.resizeTty(ctx, execID, isExec, headers)
			}
		}()
	}
	return nil
}

// GetTtySize returns the height and width in characters of the tty
func (cli *DockerCli) getTtySize() (int, int) {
	if !cli.isTerminalOut {
		return 0, 0
	}
	ws, err := term_getWinsize(cli.outFd)
	if err != nil {
		fmt.Println("Error getting size: %s", err)
		if ws == nil {
			return 0, 0
		}
	}
	return int(ws.Height), int(ws.Width)
}















// Termios is the Unix API for terminal I/O.
type Termios unix.Termios
// State represents the state of the terminal.
type State struct {
	termios Termios
}

const (
	getTermios = unix.TCGETS
	setTermios = unix.TCSETS
)

// SetRawTerminal puts the terminal connected to the given file descriptor into
// raw mode and returns the previous state. On UNIX, this puts both the input
// and output into raw mode. On Windows, it only puts the input into raw mode.
func term_setRawTerminalIn(fd uintptr) (*State, error) {
	oldState, err := term_makeRaw(fd)
	if err != nil {
		return nil, err
	}
	term_handleInterrupt(fd, oldState)
	return oldState, err
}
 
// SetRawTerminalOutput puts the output of terminal connected to the given file
// descriptor into raw mode. On UNIX, this does nothing and returns nil for the
// state. On Windows, it disables LF -> CRLF translation.
func term_setRawTerminalOutput(fd uintptr) (*State, error) {
	return nil, nil
}

// MakeRaw put the terminal connected to the given file descriptor into raw
// mode and returns the previous state of the terminal so that it can be
// restored.
func term_makeRaw(fd uintptr) (*State, error) {
	termios, err := unix.IoctlGetTermios(int(fd), getTermios)
	if err != nil {
		return nil, err
	}

	var oldState State
	oldState.termios = Termios(*termios)

	termios.Iflag &^= (unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON)
	termios.Oflag &^= unix.OPOST
	termios.Lflag &^= (unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN)
	termios.Cflag &^= (unix.CSIZE | unix.PARENB)
	termios.Cflag |= unix.CS8
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(int(fd), setTermios, termios); err != nil {
		return nil, err
	}
	return &oldState, nil
}


func term_handleInterrupt(fd uintptr, state *State) {
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt)
	go func() {
		for range sigchan {
			// quit cleanly and the new terminal item is on a new line
			fmt.Println()
			signal.Stop(sigchan)
			close(sigchan)
			term_restoreTerminal(fd, state)
			os.Exit(1)
		}
	}()
}

var (
	// ErrInvalidState is returned if the state of the terminal is invalid.
	ErrInvalidState = errors.New("Invalid terminal state")
)
// RestoreTerminal restores the terminal connected to the given file descriptor
// to a previous state.
func term_restoreTerminal(fd uintptr, state *State) error {
	if state == nil {
		return ErrInvalidState
	}
	if err := tcset(fd, &state.termios); err != 0 {
		return err
	}
	return nil
}

// GetFdInfo returns the file descriptor for an os.File and indicates whether the file represents a terminal.
func term_getFdInfo(in interface{}) (uintptr, bool) {
	var inFd uintptr
	var isTerminalIn bool
	if file, ok := in.(*os.File); ok {
		inFd = file.Fd()
		isTerminalIn = term_isTerminal(inFd)
	}
	return inFd, isTerminalIn
}

// IsTerminal returns true if the given file descriptor is a terminal.
func term_isTerminal(fd uintptr) bool {
	var termios Termios
	return tcget(fd, &termios) == 0
}

func tcget(fd uintptr, p *Termios) syscall.Errno {
	_, _, err := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(getTermios), uintptr(unsafe.Pointer(p)))
	return err
}

func tcset(fd uintptr, p *Termios) syscall.Errno {
	_, _, err := unix.Syscall(unix.SYS_IOCTL, fd, setTermios, uintptr(unsafe.Pointer(p)))
	return err
}


// Winsize is used for window size.
type Winsize struct {
	Height uint16
	Width  uint16
}
// GetWinsize returns the window size based on the specified file descriptor.
func term_getWinsize(fd uintptr) (*Winsize, error) {
	ws := &Winsize{}
	_, _, err := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(unix.TIOCGWINSZ), uintptr(unsafe.Pointer(ws)))
	// Skipp errno = 0
	if err == 0 {
		return ws, nil
	}
	return ws, err
}















// StdType is the type of standard stream
// a writer can multiplex to.
type StdType byte

const (
	// Stdin represents standard input stream type.
	Stdin StdType = iota
	// Stdout represents standard output stream type.
	Stdout
	// Stderr represents standard error steam type.
	Stderr
	// Systemerr represents errors originating from the system that make it
	// into the the multiplexed stream.
	Systemerr

	stdWriterPrefixLen = 8
	stdWriterFdIndex   = 0
	stdWriterSizeIndex = 4

	startingBufLen = 32*1024 + stdWriterPrefixLen + 1
)

// StdCopy is a modified version of io.Copy.
//
// StdCopy will demultiplex `src`, assuming that it contains two streams,
// previously multiplexed together using a StdWriter instance.
// As it reads from `src`, StdCopy will write to `dstout` and `dsterr`.
//
// StdCopy will read until it hits EOF on `src`. It will then return a nil error.
// In other words: if `err` is non nil, it indicates a real underlying error.
//
// `written` will hold the total number of bytes written to `dstout` and `dsterr`.
func stdCopy(dstout, dsterr io.Writer, src io.Reader) (written int64, err error) {
	var (
		buf       = make([]byte, startingBufLen)
		bufLen    = len(buf)
		nr, nw    int
		er, ew    error
		out       io.Writer
		frameSize int
	)

	for {
		// Make sure we have at least a full header
		for nr < stdWriterPrefixLen {
			var nr2 int
			nr2, er = src.Read(buf[nr:])
			nr += nr2
			if er == io.EOF {
				if nr < stdWriterPrefixLen {
					return written, nil
				}
				break
			}
			if er != nil {
				return 0, er
			}
		}

		stream := StdType(buf[stdWriterFdIndex])
		// Check the first byte to know where to write
		switch stream {
		case Stdin:
			fallthrough
		case Stdout:
			// Write on stdout
			out = dstout
		case Stderr:
			// Write on stderr
			out = dsterr
		case Systemerr:
			// If we're on Systemerr, we won't write anywhere.
			// NB: if this code changes later, make sure you don't try to write
			// to outstream if Systemerr is the stream
			out = nil
		default:
			return 0, fmt.Errorf("Unrecognized input header: %d", buf[stdWriterFdIndex])
		}

		// Retrieve the size of the frame
		frameSize = int(binary.BigEndian.Uint32(buf[stdWriterSizeIndex : stdWriterSizeIndex+4]))

		// Check if the buffer is big enough to read the frame.
		// Extend it if necessary.
		if frameSize+stdWriterPrefixLen > bufLen {
			buf = append(buf, make([]byte, frameSize+stdWriterPrefixLen-bufLen+1)...)
			bufLen = len(buf)
		}

		// While the amount of bytes read is less than the size of the frame + header, we keep reading
		for nr < frameSize+stdWriterPrefixLen {
			var nr2 int
			nr2, er = src.Read(buf[nr:])
			nr += nr2
			if er == io.EOF {
				if nr < frameSize+stdWriterPrefixLen {
					return written, nil
				}
				break
			}
			if er != nil {
				return 0, er
			}
		}

		// we might have an error from the source mixed up in our multiplexed
		// stream. if we do, return it.
		if stream == Systemerr {
			return written, fmt.Errorf("error from daemon in stream: %s", string(buf[stdWriterPrefixLen:frameSize+stdWriterPrefixLen]))
		}

		// Write the retrieved frame (without header)
		nw, ew = out.Write(buf[stdWriterPrefixLen : frameSize+stdWriterPrefixLen])
		if ew != nil {
			return 0, ew
		}

		// If the frame has not been fully written: error
		if nw != frameSize {
			return 0, io.ErrShortWrite
		}
		written += int64(nw)

		// Move the rest of the buffer to the beginning
		copy(buf, buf[frameSize+stdWriterPrefixLen:])
		// Move the index
		nr -= frameSize + stdWriterPrefixLen
	}
}
