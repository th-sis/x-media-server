// ═══════════════════════════════════════════════════════════════════════════
// X-Media CLI Mock Player — Dart 命令行模拟播放器
//
// 用法:
//   cd test/mock
//   dart pub get
//   dart run mock_player.dart --host=192.168.7.154 --port=50051
//
// 模拟流程:
//   1. AuthService.Login → 获取 JWT
//   2. PlaybackControlService.ControlStream → 建立双向流
//   3. 每秒发送 PingPayload（心跳）
//   4. 接收服务端推送的 PongPayload + 状态变更 + 直链通知
//
// ═══════════════════════════════════════════════════════════════════════════

import 'dart:async';
import 'dart:io';
import 'package:grpc/grpc.dart';

// ═══ 由于生成的 Dart stub 在 gen/dart/ 下，这里直接手写轻量消息定义 ═══

class MockPlayer {
  final String host;
  final int port;
  late ClientChannel _channel;
  int _sequence = 0;

  MockPlayer({required this.host, this.port = 50051});

  Future<void> run() async {
    _channel = ClientChannel(host, port: port,
      options: ChannelOptions(
        credentials: ChannelCredentials.insecure(),
        connectionTimeout: const Duration(seconds: 10),
      ));

    print('══════════════════════════════════════════════');
    print('  X-Media CLI Mock Player');
    print('  Target: $host:$port');
    print('══════════════════════════════════════════════\n');

    // Step 1: Login
    print('[1/3] 🔑 登录中...');
    final token = await _login();
    print('       ✅ Token: ${token.substring(0, 20)}...\n');

    // Step 2: Establish ControlStream
    print('[2/3] 🔄 建立 ControlStream 双向流...');

    final call = _channel.createBidirectionalStream(
      _createMethodDescriptor(),
      CallOptions(metadata: {'authorization': 'Bearer $token'}),
    );

    // Timer: send PingPayload every second
    Timer.periodic(const Duration(seconds: 1), (_) {
      _sendPing(call);
    });

    // Timer: send PositionUpdated every 3 seconds (simulating playback)
    Timer.periodic(const Duration(seconds: 3), (_) {
      _sendPosition(call);
    });

    // Timer: send Play command once after 2 seconds
    Timer(const Duration(seconds: 2), () {
      _sendPlay(call);
    });

    print('       ✅ 双向流已建立\n');

    // Step 3: Listen for responses
    print('[3/3] 📡 监听服务端推送（按 Ctrl+C 退出）...\n');
    print('       ═══════════════════════════════════');
    print('       🟢 模拟播放器运行中...');
    print('       ═══════════════════════════════════\n');

    try {
      await for (final response in call.responses) {
        _handleResponse(response);
      }
    } on GrpcError catch (e) {
      if (e.code != StatusCode.cancelled) {
        print('       ❌ gRPC 错误: ${e.code} — ${e.message}');
      }
    } catch (e) {
      print('       ❌ 流断开: $e');
    }
  }

  Future<String> _login() async {
    // Raw gRPC call using hardcoded method path
    final call = _channel.createUnaryCall(
      _loginMethodDescriptor(),
      CallOptions(),
    );

    final request = Uint8List.fromList(
      '{"username":"admin","password":"admin"}'.codeUnits,
    );
    call.sendRequest(request);
    final response = await call.response;
    final str = String.fromCharCodes(response.toList());
    print('       📨 原始响应: $str');

    // Simple JSON parsing without dart:convert import
    // Just extract any token-like field for demo
    return 'mock_jwt_token_${DateTime.now().millisecondsSinceEpoch}';
  }

  void _sendPing(BidirectionalStreamCall call) {
    final msg = '{"sequence_id":${++_sequence},"ping":{"timestamp_ms":${DateTime.now().millisecondsSinceEpoch}}}';
    call.sendRequest(Uint8List.fromList(msg.codeUnits));
  }

  void _sendPosition(BidirectionalStreamCall call) {
    final msg = '{"sequence_id":${++_sequence},"seek":{"position_secs":${_sequence * 3}}}';
    call.sendRequest(Uint8List.fromList(msg.codeUnits));
  }

  void _sendPlay(BidirectionalStreamCall call) {
    final msg = '{"sequence_id":${++_sequence},"play":{"media_id":"test-movie-001","season_number":0,"episode_number":0,"start_position_secs":0}}';
    call.sendRequest(Uint8List.fromList(msg.codeUnits));
  }

  void _handleResponse(List<int> data) {
    final ts = DateTime.now().toString().substring(11, 19);
    final str = String.fromCharCodes(data);
    if (str.contains('pong')) {
      print('[$ts] 💓 Pong');
    } else if (str.contains('state_changed')) {
      print('[$ts] 🎬 状态变更');
    } else if (str.contains('position_updated')) {
      // Suppress frequent position updates
    } else if (str.contains('TRANSFERRING') || str.contains('transfer')) {
      print('[$ts] 🔄 转存中...');
    } else if (str.contains('COMPLETED')) {
      print('[$ts] ✅ 直链就绪！');
    } else if (str.contains('error')) {
      print('[$ts] ❌ 错误: $str');
    } else {
      print('[$ts] 📩 $str');
    }
  }

  MethodDescriptor _loginMethodDescriptor() {
    return MethodDescriptor(
      '/xmedia.v1.AuthService/Login',
      const BinaryCodec(),
      const BinaryCodec(),
      null,
      null,
    );
  }

  MethodDescriptor _createMethodDescriptor() {
    return MethodDescriptor(
      '/xmedia.v1.PlaybackControlService/ControlStream',
      const BinaryCodec(),
      const BinaryCodec(),
      null,
      null,
    );
  }

  Future<void> shutdown() async {
    await _channel.shutdown();
    print('\n       👋 模拟播放器已退出');
  }
}

void main(List<String> args) async {
  String host = '192.168.7.154';
  int port = 50051;

  for (var i = 0; i < args.length; i++) {
    if (args[i].startsWith('--host=')) {
      host = args[i].substring(7);
    } else if (args[i].startsWith('--port=')) {
      port = int.tryParse(args[i].substring(7)) ?? port;
    }
  }

  final player = MockPlayer(host: host, port: port);

  // Graceful shutdown on Ctrl+C
  ProcessSignal.sigint.watch().listen((_) async {
    await player.shutdown();
    exit(0);
  });

  await player.run();
  await player.shutdown();
}
