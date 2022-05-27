package meshnet

import (
	"context"
	"net"
	"os"

	"github.com/networkop/meshnet-cni/daemon/grpcwire"
	"github.com/networkop/meshnet-cni/daemon/vxlan"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"github.com/google/gopacket/pcap"
	mpb "github.com/networkop/meshnet-cni/daemon/proto/meshnet/v1beta1"
)

func (m *Meshnet) getPod(ctx context.Context, name, ns string) (*unstructured.Unstructured, error) {
	log.Infof("Reading pod %s from K8s", name)
	return m.tClient.Topology(ns).Unstructured(ctx, name, metav1.GetOptions{})
}

func (m *Meshnet) updateStatus(ctx context.Context, obj *unstructured.Unstructured, ns string) error {
	log.Infof("Update pod status %s from K8s", obj.GetName())
	_, err := m.tClient.Topology(ns).Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

func (m *Meshnet) Get(ctx context.Context, pod *mpb.PodQuery) (*mpb.Pod, error) {
	log.Infof("Retrieving %s's metadata from K8s...", pod.Name)

	result, err := m.getPod(ctx, pod.Name, pod.KubeNs)
	if err != nil {
		log.Errorf("Failed to read pod %s from K8s", pod.Name)
		return nil, err
	}

	remoteLinks, found, err := unstructured.NestedSlice(result.Object, "spec", "links")
	if err != nil || !found || remoteLinks == nil {
		log.Errorf("Could not find 'Link' array in pod's spec")
		return nil, err
	}

	links := make([]*mpb.Link, len(remoteLinks))
	for i := range links {
		remoteLink, ok := remoteLinks[i].(map[string]interface{})
		if !ok {
			log.Errorf("Unrecognised 'Link' structure")
			return nil, err
		}
		newLink := &mpb.Link{}
		newLink.PeerPod, _, _ = unstructured.NestedString(remoteLink, "peer_pod")
		newLink.PeerIntf, _, _ = unstructured.NestedString(remoteLink, "peer_intf")
		newLink.LocalIntf, _, _ = unstructured.NestedString(remoteLink, "local_intf")
		newLink.LocalIp, _, _ = unstructured.NestedString(remoteLink, "local_ip")
		newLink.PeerIp, _, _ = unstructured.NestedString(remoteLink, "peer_ip")
		newLink.Uid, _, _ = unstructured.NestedInt64(remoteLink, "uid")
		links[i] = newLink
	}

	srcIP, _, _ := unstructured.NestedString(result.Object, "status", "src_ip")
	netNs, _, _ := unstructured.NestedString(result.Object, "status", "net_ns")
	nodeIP := os.Getenv("HOST_IP")

	return &mpb.Pod{
		Name:   pod.Name,
		SrcIp:  srcIP,
		NetNs:  netNs,
		KubeNs: pod.KubeNs,
		Links:  links,
		NodeIp: nodeIP,
	}, nil
}

func (m *Meshnet) SetAlive(ctx context.Context, pod *mpb.Pod) (*mpb.BoolResponse, error) {
	log.Infof("Setting %s's SrcIp=%s and NetNs=%s", pod.Name, pod.SrcIp, pod.NetNs)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result, err := m.getPod(ctx, pod.Name, pod.KubeNs)
		if err != nil {
			log.Errorf("Failed to read pod %s from K8s", pod.Name)
			return err
		}

		if err = unstructured.SetNestedField(result.Object, pod.SrcIp, "status", "src_ip"); err != nil {
			log.Errorf("Failed to update pod's src_ip")
		}

		if err = unstructured.SetNestedField(result.Object, pod.NetNs, "status", "net_ns"); err != nil {
			log.Errorf("Failed to update pod's net_ns")
		}

		return m.updateStatus(ctx, result, pod.KubeNs)
	})

	if retryErr != nil {
		log.WithFields(log.Fields{
			"err":      retryErr,
			"function": "SetAlive",
		}).Errorf("Failed to update pod %s alive status", pod.Name)
		return &mpb.BoolResponse{Response: false}, retryErr
	}

	return &mpb.BoolResponse{Response: true}, nil
}

func (m *Meshnet) Skip(ctx context.Context, skip *mpb.SkipQuery) (*mpb.BoolResponse, error) {
	log.Infof("Skipping of pod %s by pod %s", skip.Peer, skip.Pod)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result, err := m.getPod(ctx, skip.Pod, skip.KubeNs)
		if err != nil {
			log.Errorf("Failed to read pod %s from K8s", skip.Pod)
			return err
		}

		skipped, _, _ := unstructured.NestedSlice(result.Object, "status", "skipped")

		newSkipped := append(skipped, skip.Peer)

		if err := unstructured.SetNestedField(result.Object, newSkipped, "status", "skipped"); err != nil {
			log.Errorf("Failed to updated skipped list")
			return err
		}

		return m.updateStatus(ctx, result, skip.KubeNs)
	})
	if retryErr != nil {
		log.WithFields(log.Fields{
			"err":      retryErr,
			"function": "Skip",
		}).Errorf("Failed to update skip pod %s status", skip.Pod)
		return &mpb.BoolResponse{Response: false}, retryErr
	}

	return &mpb.BoolResponse{Response: true}, nil
}

func (m *Meshnet) SkipReverse(ctx context.Context, skip *mpb.SkipQuery) (*mpb.BoolResponse, error) {
	log.Infof("Reverse-skipping of pod %s by pod %s", skip.Peer, skip.Pod)

	var podName string
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// setting the value for peer pod
		peerPod, err := m.getPod(ctx, skip.Peer, skip.KubeNs)
		if err != nil {
			log.Errorf("Failed to read pod %s from K8s", skip.Pod)
			return err
		}
		podName = peerPod.GetName()

		// extracting peer pod's skipped list and adding this pod's name to it
		peerSkipped, _, _ := unstructured.NestedSlice(peerPod.Object, "status", "skipped")
		newPeerSkipped := append(peerSkipped, skip.Pod)

		log.Infof("Updating peer skipped list")
		// updating peer pod's skipped list locally
		if err := unstructured.SetNestedField(peerPod.Object, newPeerSkipped, "status", "skipped"); err != nil {
			log.Errorf("Failed to updated reverse-skipped list for peer pod %s", peerPod.GetName())
			return err
		}

		// sending peer pod's updates to k8s
		return m.updateStatus(ctx, peerPod, skip.KubeNs)
	})
	if retryErr != nil {
		log.WithFields(log.Fields{
			"err":      retryErr,
			"function": "SkipReverse",
		}).Errorf("Failed to update peer pod %s skipreverse status", podName)
		return &mpb.BoolResponse{Response: false}, retryErr
	}

	retryErr = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// setting the value for this pod
		thisPod, err := m.getPod(ctx, skip.Pod, skip.KubeNs)
		if err != nil {
			log.Errorf("Failed to read pod %s from K8s", skip.Pod)
			return err
		}

		// extracting this pod's skipped list and removing peer pod's name from it
		thisSkipped, _, _ := unstructured.NestedSlice(thisPod.Object, "status", "skipped")
		newThisSkipped := make([]interface{}, 0)

		log.WithFields(log.Fields{
			"thisSkipped": thisSkipped,
		}).Info("THIS SKIPPED:")

		for _, el := range thisSkipped {
			elString, ok := el.(string)
			if ok {
				if elString != skip.Peer {
					log.Errorf("Appending new element %s", elString)
					newThisSkipped = append(newThisSkipped, elString)
				}
			}
		}

		log.WithFields(log.Fields{
			"newThisSkipped": newThisSkipped,
		}).Info("NEW THIS SKIPPED:")

		// updating this pod's skipped list locally
		if len(newThisSkipped) != 0 {
			if err := unstructured.SetNestedField(thisPod.Object, newThisSkipped, "status", "skipped"); err != nil {
				log.Errorf("Failed to cleanup skipped list for pod %s", thisPod.GetName())
				return err
			}

			// sending this pod's updates to k8s
			return m.updateStatus(ctx, thisPod, skip.KubeNs)
		}
		return nil
	})
	if retryErr != nil {
		log.WithFields(log.Fields{
			"err":      retryErr,
			"function": "SkipReverse",
		}).Error("Failed to update this pod skipreverse status")
		return &mpb.BoolResponse{Response: false}, retryErr
	}

	return &mpb.BoolResponse{Response: true}, nil
}

func (m *Meshnet) IsSkipped(ctx context.Context, skip *mpb.SkipQuery) (*mpb.BoolResponse, error) {
	log.Infof("Checking if %s is skipped by %s", skip.Peer, skip.Pod)

	result, err := m.getPod(ctx, skip.Peer, skip.KubeNs)
	if err != nil {
		log.Errorf("Failed to read pod %s from K8s", skip.Pod)
		return nil, err
	}

	skipped, _, _ := unstructured.NestedSlice(result.Object, "status", "skipped")

	for _, peer := range skipped {
		if skip.Pod == peer.(string) {
			return &mpb.BoolResponse{Response: true}, nil
		}
	}

	return &mpb.BoolResponse{Response: false}, nil
}

func (m *Meshnet) Update(ctx context.Context, pod *mpb.RemotePod) (*mpb.BoolResponse, error) {
	if err := vxlan.CreateOrUpdate(pod); err != nil {
		log.Errorf("Failed to Update Vxlan")
		return &mpb.BoolResponse{Response: false}, nil
	}
	return &mpb.BoolResponse{Response: true}, nil
}

//------------------------------------------------------------------------------------------------------
func (m *Meshnet) RemGRPCWire(ctx context.Context, wireDef *mpb.WireDef) (*mpb.BoolResponse, error) {
	log.Info("============RemGRPCWire==start==================")
	defer log.Info("++++++++++++RemGRPCWire++end++++++++++++++++++++")

	resp := true
	err := grpcwire.RemWireFrmPod(wireDef.KubeNs, wireDef.LocalPodNm)
	if err != nil {
		resp = false
	}
	return &mpb.BoolResponse{Response: resp}, nil
}

//------------------------------------------------------------------------------------------------------
func (m *Meshnet) AddGRPCWireLocal(ctx context.Context, wireDef *mpb.WireDef) (*mpb.BoolResponse, error) {

	log.Infof("============Daemon-Service-AddWireLocal(Start), wire ID - %v, Peer Machine IP - %v =============", wireDef.PeerIntfId, wireDef.PeerIp)
	defer log.Infof("=x=x=x=x=x=x=x===Daemon-Service-AddWireLocal(End, wire ID - %v===x=x=x=x=x=x=x=", wireDef.PeerIntfId)

	locInf, err := net.InterfaceByName(wireDef.VethNameLocalHost)
	if err != nil {
		log.Errorf("Failed to retrive interface ID for interface %v. error:%v", wireDef.VethNameLocalHost, err)
		return &mpb.BoolResponse{Response: false}, err
	}

	handle, err := pcap.OpenLive(wireDef.VethNameLocalHost, 1600, true, pcap.BlockForever)
	if err != nil {
		log.Fatalf("Could not open interface for send/recv packets for containers. error:%v", err)
		return &mpb.BoolResponse{Response: false}, err
	}

	aWire := grpcwire.GRPCWire{
		Uid: int(wireDef.LinkUid),

		LocalNodeIntfID: int64(locInf.Index),
		LocalNodeIntfNm: wireDef.VethNameLocalHost,
		LocalPodIP:      "Not Available",
		LocalPodIntfNm:  wireDef.IntfNameInPod,
		LocalPodNm:      wireDef.LocalPodNm,
		LocalPodNetNS:   wireDef.LocalPodNetNs,

		PeerInffID: wireDef.PeerIntfId,
		PeerPodIP:  wireDef.PeerIp,

		IsReady:       true,
		HowCreated:    grpcwire.HOST_CREATED_WIRE,
		CreaterHostIP: "unknown", /*+++king(todo) retrive host ip and set it here vxlan.HOST_CREATED_WIRE*/

		StopC:     make(chan bool),
		Namespace: wireDef.KubeNs,
	}

	grpcwire.AddActiveWire(&aWire, handle)

	log.Infof("Starting the local packet receive thread for pod interface %s", wireDef.IntfNameInPod)
	//go grpcwire.RecvFrmLocalPodThread(wireDef.PeerIp, wireDef.PeerIntfId, wireDef.VethNameLocalHost, &aWire.StopC)
	go grpcwire.RecvFrmLocalPodThread(&aWire)

	return &mpb.BoolResponse{Response: true}, nil
}

//------------------------------------------------------------------------------------------------------
func (m *Meshnet) SendToOnce(ctx context.Context, pkt *mpb.Packet) (*mpb.BoolResponse, error) {
	//log.Infof("============Daemon-Service-SendToOnce(Start), wire ID - %v Pkt Length- %v =============", pkt.RemotIntfId, pkt.FrameLen)

	//defer log.Infof("=x=x=x=x=x=x=x===Daemon-Service-SendToOnce(End, wire ID - %v===x=x=x=x=x=x=x=", pkt.RemotIntfId)

	// Unpack Ethernet frame into Go representation.
	// var eFrame ethernet.Frame
	// if err := (&eFrame).UnmarshalBinary(pkt.Frame[:pkt.FrameLen]); err != nil {
	// 	log.Fatalf("+++Daemon: failed to unmarshal ethernet frame: %v", err)
	// }

	//return &mpb.BoolResponse{Response: true}, nil

	handle, err := grpcwire.GetHostIntfHndl(pkt.RemotIntfId)
	if err != nil {
		log.Printf("+++Daemon-Service-SendToOnce (wire id - %v): Could not find local handle. err:%v", pkt.RemotIntfId, err)
		return &mpb.BoolResponse{Response: false}, err
	}
	pktType := grpcwire.DecodePkt(pkt.Frame)

	log.Printf("+++Daemon(SendToOnce): Received [pkt: %s, bytes: %d, for local interface id: %d]. Sending it to local container", pktType, pkt.FrameLen, pkt.RemotIntfId)

	if pkt.FrameLen <= 1518 {
		err = handle.WritePacketData(pkt.Frame)
		if err != nil {
			log.Printf("+++Daemon-Service-SendToOnce (wire id - %v): Could not write packet(%d bytes) to local interface. err:%v", pkt.RemotIntfId, pkt.FrameLen, err)
			return &mpb.BoolResponse{Response: false}, err
		}
	} else {
		log.Printf("+++Daemon-Service-SendToOnce (wire id - %v): Received unusually large size packet(%d bytes) from peer. Not delivering it to local pod")

	}

	/* +++king(Input)
	InterfaceByIndex(index int) (*Interface, error)

	looks like pcap handle and pcap write packet data is one way to do.
	PCAP REf:-
	https://austinmarton.wordpress.com/2011/09/14/sending-raw-ethernet-packets-from-a-specific-interface-in-c-on-linux/
	https://www.devdungeon.com/content/packet-capture-injection-and-analysis-gopacket#creating-sending-packets
	https://github.com/jesseward/gopacket/blob/master/examples/pcaplay/main.go
	https://golang.hotexamples.com/examples/github.com.google.gopacket.pcap/Handle/WritePacketData/golang-handle-writepacketdata-method-examples.html

	GOPACKET Ref :-
	https://github.com/google/gopacket/blob/master/afpacket/afpacket.go
	https://stackoverflow.com/questions/61090243/read-from-a-raw-socket-connected-to-a-network-interface-using-golang
	https://css.bz/2016/12/08/go-raw-sockets.html
	https://golang.hotexamples.com/examples/syscall/-/BindToDevice/golang-bindtodevice-function-examples.html

	Use capture --> https://github.com/ghedo/go.pkt
	Use Channel --> https://stackoverflow.com/questions/62534827/no-blocking-eternet-capture

	*/

	return &mpb.BoolResponse{Response: true}, nil
}

//---------------------------------------------------------------------------------------------------------------

func (m *Meshnet) AddGRPCWireRemote(ctx context.Context, wireDef *mpb.WireDef) (*mpb.WireCreateResponse, error) {

	stopC := make(chan (bool))
	wire, err := grpcwire.CreateGRPCWireRemoteTriggered(wireDef, &stopC)

	if err == nil {
		//go grpcwire.RecvFrmLocalPodThread(wireDef.PeerIp, wireDef.PeerIntfId, localHostVethName, &stopC)
		go grpcwire.RecvFrmLocalPodThread(wire)

		//return &mpb.WireCreateResponse{Response: true, PeerIntfId:  localHostVethIndex}, nil
		return &mpb.WireCreateResponse{Response: true, PeerIntfId: wire.LocalNodeIntfID}, nil
	}
	log.Errorf("AddWireRemote err : %v", err)
	return &mpb.WireCreateResponse{Response: false, PeerIntfId: wire.LocalNodeIntfID}, err
}

//---------------------------------------------------------------------------------------------------------------
func (m *Meshnet) GenLocVEthID(ctx context.Context, in *mpb.ReqIntfID) (*mpb.RespIntfID, error) {
	id := grpcwire.GetNextIndex()
	//log.Infof("+++Daemon:(GenLocVEthID RPC) : called ot get external vthe pair creation corespoding to %s, returning id %d", in.InContIntfNm, id)
	return &mpb.RespIntfID{Ok: true, LocalIntfId: id}, nil
}

//---------------------------------------------------------------------------------------------------------------
func (m *Meshnet) GRPCWireExists(ctx context.Context, wireDef *mpb.WireDef) (*mpb.WireCreateResponse, error) {

	wire, ok := grpcwire.GetActiveWire(int(wireDef.LinkUid), wireDef.LocalPodNetNs)

	if ok && wire != nil {
		return &mpb.WireCreateResponse{Response: ok, PeerIntfId: (*wire).PeerInffID}, nil
	}

	return &mpb.WireCreateResponse{Response: false, PeerIntfId: 0}, nil

}
